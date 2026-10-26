// Package engine schedules workflow nodes, tracks their states, and
// publishes every change as an event.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// Runner runs one node attempt. runner.Process is the real implementation.
type Runner interface {
	Run(ctx context.Context, spec runner.Spec) runner.Result
}

// TerminalLender gives an interactive node exclusive use of the terminal.
// Lend runs fn with the terminal handed over and returns once fn returns
// and the UI has the terminal back. It returns an error, without calling
// fn, when no terminal is available.
type TerminalLender interface {
	Lend(fn func()) error
}

// Options configures an Engine.
type Options struct {
	Workflow *workflow.Workflow
	Run      *store.Run
	Runner   Runner
	Lender   TerminalLender
	Bus      *events.Bus
	// Concurrency overrides the workflow's limit when positive.
	Concurrency int
	// Env is the base environment for every node, usually os.Environ plus
	// color settings.
	Env []string
	Log zerolog.Logger
}

// Summary is the outcome of a run.
type Summary struct {
	Success bool
	States  map[string]events.State
}

// Counts returns how many nodes ended in each state.
func (s Summary) Counts() map[events.State]int {
	out := map[events.State]int{}
	for _, st := range s.States {
		out[st]++
	}
	return out
}

type nodeRun struct {
	node     *workflow.Node
	state    events.State
	attempt  int
	started  time.Time
	finished time.Time
	cancel   context.CancelFunc
	kill     chan struct{}
}

type finished struct {
	id     string
	result runner.Result
}

// Engine runs one workflow. Use it for a single Run call.
type Engine struct {
	opts        Options
	concurrency int
	nodes       map[string]*nodeRun
	log         zerolog.Logger

	cancelOnce sync.Once
	cancelCh   chan struct{}
	killOnce   sync.Once
	killCh     chan struct{}
}

// New returns an engine for opts.
func New(opts Options) *Engine {
	concurrency := opts.Workflow.Concurrency
	if opts.Concurrency > 0 {
		concurrency = opts.Concurrency
	}
	nodes := make(map[string]*nodeRun, len(opts.Workflow.Nodes))
	for _, n := range opts.Workflow.Nodes {
		nodes[n.ID] = &nodeRun{
			node:     n,
			state:    events.Pending,
			attempt:  1,
			started:  time.Time{},
			finished: time.Time{},
			cancel:   nil,
			kill:     nil,
		}
	}
	return &Engine{
		opts:        opts,
		concurrency: concurrency,
		nodes:       nodes,
		log:         opts.Log,
		cancelOnce:  sync.Once{},
		cancelCh:    make(chan struct{}),
		killOnce:    sync.Once{},
		killCh:      make(chan struct{}),
	}
}

// Cancel stops new nodes from starting and starts graceful shutdown of the
// running ones. It is safe to call more than once and from any goroutine.
func (e *Engine) Cancel() {
	e.cancelOnce.Do(func() { close(e.cancelCh) })
}

// Kill cancels the run and skips the remaining grace periods: running nodes
// get SIGKILL at once.
func (e *Engine) Kill() {
	e.Cancel()
	e.killOnce.Do(func() { close(e.killCh) })
}

// Run executes the workflow and returns when every node has reached a
// terminal state. Canceling ctx has the same effect as Cancel.
func (e *Engine) Run(ctx context.Context) Summary {
	wf := e.opts.Workflow
	e.log.Info().Str("workflow", wf.Name).Int("nodes", len(wf.Nodes)).Int("concurrency", e.concurrency).Msg("run started")
	e.publish(events.RunStarted{Workflow: wf.Name, RunID: e.opts.Run.ID, RunDir: e.opts.Run.Dir, At: time.Now()})
	for _, n := range wf.Nodes {
		e.publishNode(e.nodes[n.ID], "", runner.Result{ExitCode: 0, Signal: "", Reason: "", Err: nil})
	}

	results := make(chan finished)
	running := 0
	canceling := false
	ctxDone := ctx.Done()
	cancelCh := e.cancelCh
	killCh := e.killCh

	for {
		// Checked before scheduling, not only in the select, so a cancel
		// that lands together with a result never lets a new node start.
		if !canceling && (ctx.Err() != nil || isClosed(e.cancelCh)) {
			canceling = true
			ctxDone, cancelCh = nil, nil
			e.log.Info().Int("running", running).Msg("cancel requested; sending SIGINT to running nodes")
			for _, nr := range e.ordered() {
				if nr.state == events.Running {
					nr.cancel()
				}
			}
		}
		if canceling {
			e.cancelUnstarted()
		} else {
			running += e.schedule(ctx, results, running)
		}
		if running == 0 {
			break
		}

		select {
		case f := <-results:
			running--
			e.finish(f.id, f.result)
			if !canceling {
				e.log.Debug().Str("node", f.id).Int("running", running).Msg("slot freed")
			}
		case <-ctxDone:
		case <-cancelCh:
		case <-killCh:
			killCh = nil
			e.log.Warn().Int("running", running).Msg("kill requested; sending SIGKILL to running nodes")
			for _, nr := range e.ordered() {
				if nr.state == events.Running {
					close(nr.kill)
				}
			}
		}
	}

	summary := Summary{Success: true, States: make(map[string]events.State, len(e.nodes))}
	for id, nr := range e.nodes {
		summary.States[id] = nr.state
		if !e.counts(nr.state, nr.node) {
			summary.Success = false
		}
	}
	e.log.Info().Bool("success", summary.Success).Interface("counts", summary.Counts()).Msg("run finished")
	e.publish(events.RunFinished{Success: summary.Success, At: time.Now()})
	return summary
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// counts reports whether a node's final state leaves the run successful.
func (e *Engine) counts(state events.State, n *workflow.Node) bool {
	switch state {
	case events.Succeeded, events.Skipped:
		return true
	case events.Failed:
		return n.ContinueOnError
	default:
		return false
	}
}

func (e *Engine) ordered() []*nodeRun {
	out := make([]*nodeRun, 0, len(e.opts.Workflow.Nodes))
	for _, n := range e.opts.Workflow.Nodes {
		out = append(out, e.nodes[n.ID])
	}
	return out
}

// satisfied reports whether a need in this state lets a dependent run.
func (e *Engine) satisfied(need *nodeRun) bool {
	switch need.state {
	case events.Succeeded, events.Skipped:
		return true
	case events.Failed:
		return need.node.ContinueOnError
	default:
		return false
	}
}

// blocks reports whether a need in this state can never be satisfied.
func (e *Engine) blocks(need *nodeRun) bool {
	switch need.state {
	case events.Failed:
		return !need.node.ContinueOnError
	case events.Canceled, events.Blocked:
		return true
	default:
		return false
	}
}

// resolve moves pending nodes to ready or blocked until nothing changes.
// Blocking propagates through chains, and declaration order need not be
// topological, so one pass is not enough.
func (e *Engine) resolve() {
	for changed := true; changed; {
		changed = false
		for _, nr := range e.ordered() {
			if nr.state != events.Pending {
				continue
			}
			ready := true
			for _, id := range nr.node.Needs {
				need := e.nodes[id]
				if e.blocks(need) {
					e.log.Debug().Str("node", nr.node.ID).Str("need", id).Str("need_state", string(need.state)).Msg("node blocked")
					e.block(nr, fmt.Sprintf("%s %s", id, need.state))
					changed = true
					ready = false
					break
				}
				if !e.satisfied(need) {
					ready = false
				}
			}
			if ready {
				e.log.Debug().Str("node", nr.node.ID).Strs("needs", nr.node.Needs).Msg("node ready: needs satisfied")
				nr.state = events.Ready
				e.publishNode(nr, "", runner.Result{ExitCode: 0, Signal: "", Reason: "", Err: nil})
				changed = true
			}
		}
	}
}

// schedule resolves states and starts whatever may start now. It returns how
// many nodes it started.
func (e *Engine) schedule(ctx context.Context, results chan<- finished, running int) int {
	e.resolve()

	var ready []*nodeRun
	for _, nr := range e.ordered() {
		if nr.state == events.Ready || nr.state == events.Waiting {
			ready = append(ready, nr)
		}
	}
	if len(ready) == 0 {
		return 0
	}

	for _, nr := range e.ordered() {
		if nr.state == events.Running && nr.node.Interactive {
			return 0
		}
	}

	// An interactive node that is ready takes priority: nothing new starts
	// until the running nodes drain and it has the terminal. Without this,
	// a steady stream of short nodes could hold it off indefinitely.
	for _, nr := range ready {
		if !nr.node.Interactive {
			continue
		}
		if running > 0 {
			if nr.state != events.Waiting {
				e.log.Debug().Str("node", nr.node.ID).Int("running", running).Msg("interactive node waiting for running nodes to finish")
				nr.state = events.Waiting
				e.publishNode(nr, "", runner.Result{ExitCode: 0, Signal: "", Reason: "", Err: nil})
			}
			return 0
		}
		e.start(ctx, nr, results)
		return 1
	}

	started := 0
	for _, nr := range ready {
		if running+started >= e.concurrency {
			e.log.Debug().Str("node", nr.node.ID).Int("running", running+started).Int("concurrency", e.concurrency).Msg("node ready but no free slot")
			break
		}
		e.start(ctx, nr, results)
		started++
	}
	return started
}

func (e *Engine) cancelUnstarted() {
	for _, nr := range e.ordered() {
		switch nr.state {
		case events.Pending, events.Ready, events.Waiting:
			e.log.Debug().Str("node", nr.node.ID).Str("from", string(nr.state)).Msg("node canceled before start")
			nr.state = events.Canceled
			nr.finished = time.Now()
			res := runner.Result{ExitCode: -1, Signal: "", Reason: runner.ReasonCanceled, Err: nil}
			e.writeRecord(nr, res, "")
			e.publishNode(nr, "canceled before start", res)
		default:
		}
	}
}

func (e *Engine) block(nr *nodeRun, why string) {
	nr.state = events.Blocked
	nr.finished = time.Now()
	res := runner.Result{ExitCode: -1, Signal: "", Reason: "blocked", Err: nil}
	e.writeRecord(nr, res, "")
	e.publishNode(nr, "blocked: "+why, res)
}

func (e *Engine) nodeLog(nr *nodeRun) zerolog.Logger {
	return e.log.With().Str("node", nr.node.ID).Int("attempt", nr.attempt).Logger()
}

func (e *Engine) start(ctx context.Context, nr *nodeRun, results chan<- finished) {
	log := e.nodeLog(nr)
	// Node contexts ignore the run context's cancellation: the main loop
	// cancels each node itself so that canceling, killing, and marking
	// unstarted nodes happen in one place and in order.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	nr.cancel = cancel
	nr.kill = make(chan struct{})
	nr.state = events.Running
	nr.started = time.Now()
	e.publishNode(nr, "", runner.Result{ExitCode: 0, Signal: "", Reason: "", Err: nil})

	spec := runner.Spec{
		Node:        nr.node.ID,
		Shell:       nr.node.Shell,
		Script:      nr.node.Run,
		Dir:         filepath.Join(e.opts.Workflow.Root, nr.node.Dir),
		Env:         e.env(nr),
		Timeout:     nr.node.Timeout,
		KillTimeout: nr.node.KillTimeout,
		TermGrace:   0,
		Output:      nil,
		Interactive: nr.node.Interactive,
		Kill:        nr.kill,
		Log:         log,
	}
	log.Info().Str("dir", spec.Dir).Bool("interactive", spec.Interactive).Msg("node started")

	logFile, err := e.opts.Run.CreateAttemptLog(nr.node.ID, nr.attempt)
	if err != nil {
		cancel()
		res := runner.Result{ExitCode: -1, Signal: "", Reason: runner.ReasonStartErr, Err: fmt.Errorf("create log file: %w", err)}
		go func() { results <- finished{id: nr.node.ID, result: res} }()
		return
	}

	id := nr.node.ID
	if nr.node.Interactive {
		go func() {
			defer cancel()
			res := e.runInteractive(ctx, spec, logFile)
			results <- finished{id: id, result: res}
		}()
		return
	}

	lines := newLineWriter(func(line string) {
		e.publish(events.NodeOutput{Node: id, Line: line})
	})
	spec.Output = io.MultiWriter(logFile, lines)
	go func() {
		defer cancel()
		res := e.opts.Runner.Run(ctx, spec)
		lines.Flush()
		if err := logFile.Close(); err != nil {
			log.Warn().Err(err).Msg("close node log")
		}
		results <- finished{id: id, result: res}
	}()
}

func (e *Engine) runInteractive(ctx context.Context, spec runner.Spec, logFile *os.File) runner.Result {
	var res runner.Result
	lendErr := errors.New("no terminal lender configured")
	if e.opts.Lender != nil {
		lendErr = e.opts.Lender.Lend(func() { res = e.opts.Runner.Run(ctx, spec) })
	}
	if lendErr != nil {
		res = runner.Result{ExitCode: -1, Signal: "", Reason: runner.ReasonStartErr, Err: lendErr}
	}
	// The child owns the terminal, so a Ctrl-C reaches it and not weft.
	// Dying of SIGINT, or exiting 130 as shells do after one, means the
	// user meant to stop, so the whole run cancels instead of carrying on
	// with independent branches.
	if res.Reason == runner.ReasonExit && (res.Signal == syscall.SIGINT.String() || res.ExitCode == 130) {
		e.log.Info().Str("node", spec.Node).Msg("interactive node interrupted from the terminal; canceling run")
		res.Reason = runner.ReasonCanceled
		e.Cancel()
	}
	_, _ = fmt.Fprintf(logFile, "weft: interactive node; output went to the terminal and was not captured\nweft: %s\n", describe(res))
	_ = logFile.Close()
	return res
}

func (e *Engine) env(nr *nodeRun) []string {
	env := make([]string, 0, len(e.opts.Env)+len(nr.node.Env)+5)
	env = append(env, e.opts.Env...)
	for k, v := range nr.node.Env {
		env = append(env, k+"="+v)
	}
	return append(env,
		"WEFT_WORKFLOW="+e.opts.Workflow.Name,
		"WEFT_ROOT="+e.opts.Workflow.Root,
		"WEFT_RUN_ID="+e.opts.Run.ID,
		"WEFT_RUN_DIR="+e.opts.Run.Dir,
		"WEFT_NODE="+nr.node.ID,
		"WEFT_ATTEMPT="+strconv.Itoa(nr.attempt),
	)
}

func (e *Engine) finish(id string, res runner.Result) {
	nr := e.nodes[id]
	nr.finished = time.Now()
	switch {
	case res.Success():
		nr.state = events.Succeeded
	case res.Reason == runner.ReasonCanceled:
		nr.state = events.Canceled
	default:
		nr.state = events.Failed
	}

	// A failed node is the workflow's outcome, not a problem in weft, so it
	// logs at info like any other transition.
	log := e.nodeLog(nr)
	log.Info().Str("state", string(nr.state)).Str("reason", string(res.Reason)).Int("exit_code", res.ExitCode).
		Str("signal", res.Signal).AnErr("error", res.Err).Dur("duration_ms", nr.finished.Sub(nr.started)).Msg("node finished")

	e.writeRecord(nr, res, e.opts.Run.AttemptLogPath(id, nr.attempt))
	errText := ""
	if nr.state != events.Succeeded {
		errText = describe(res)
	}
	e.publishNode(nr, errText, res)
}

// describe renders a result for humans: "exit code 3", "timed out", ...
func describe(res runner.Result) string {
	switch res.Reason {
	case runner.ReasonStartErr:
		return fmt.Sprintf("failed to start: %v", res.Err)
	case runner.ReasonTimeout:
		return "timed out"
	case runner.ReasonCanceled:
		if res.Signal != "" {
			return "canceled (" + res.Signal + ")"
		}
		return fmt.Sprintf("canceled (exit code %d)", res.ExitCode)
	case runner.ReasonExit:
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	if res.Signal != "" {
		return "killed by " + res.Signal
	}
	return fmt.Sprintf("exit code %d", res.ExitCode)
}

func (e *Engine) writeRecord(nr *nodeRun, res runner.Result, logPath string) {
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	rel := ""
	if logPath != "" {
		rel = e.opts.Run.RelPath(logPath)
	}
	rec := store.AttemptRecord{
		Node:        nr.node.ID,
		Attempt:     nr.attempt,
		Command:     nr.node.Run,
		Shell:       nr.node.Shell,
		Dir:         filepath.Join(e.opts.Workflow.Root, nr.node.Dir),
		Interactive: nr.node.Interactive,
		State:       string(nr.state),
		Reason:      string(res.Reason),
		ExitCode:    res.ExitCode,
		Signal:      res.Signal,
		Error:       errText,
		Started:     nr.started,
		Finished:    nr.finished,
		Log:         rel,
	}
	if err := e.opts.Run.WriteAttemptRecord(rec); err != nil {
		log := e.nodeLog(nr)
		log.Error().Err(err).Msg("write attempt record")
	}
}

func (e *Engine) publishNode(nr *nodeRun, errText string, res runner.Result) {
	exitCode := 0
	if nr.state.Terminal() {
		exitCode = res.ExitCode
	}
	e.publish(events.NodeChanged{
		Node:     nr.node.ID,
		State:    nr.state,
		Attempt:  nr.attempt,
		ExitCode: exitCode,
		Err:      errText,
		Reason:   string(res.Reason),
		Started:  nr.started,
		Finished: nr.finished,
		At:       time.Now(),
	})
}

func (e *Engine) publish(ev events.Event) {
	if e.opts.Bus != nil {
		e.opts.Bus.Publish(ev)
	}
}
