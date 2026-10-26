// Package engine schedules workflow nodes, tracks their states, and
// publishes every change as an event.
//
// A run is one or more passes. The first pass starts with Run; each pass
// ends when no node is running or can start, with a RunFinished event. An
// engine with Options.Reruns then waits for Rerun, which starts another
// pass, until Close. Rerun during a pass adds to that pass. CancelNode
// stops one node of a pass and leaves the rest of it running.
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
	"github.com/hay-kot/weft/internal/plan"
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
	// Plan sets where each node starts. A nil Plan is a fresh run.
	Plan *plan.Plan
	// State, when set, is the run's persisted state. The engine updates its
	// nodes and writes it to state.json after every change. The caller
	// fills in the rest, including the History entry for this start.
	State *store.State
	// Reruns keeps the engine taking Rerun requests after a pass ends. Run
	// then returns only after Close, or once ctx is canceled.
	Reruns bool
	Log    zerolog.Logger
}

// ErrClosed means the engine has stopped taking reruns.
var ErrClosed = errors.New("the run is over and takes no more reruns")

// ErrCanceling means a rerun arrived while the pass is being canceled.
var ErrCanceling = errors.New("the run is being canceled; rerun once it has stopped")

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
	exitCode int
	reason   string
	errText  string
	// log is the latest attempt's output log, relative to the run dir.
	log    string
	pgid   int
	cancel context.CancelFunc
	kill   chan struct{}
	// stopping is set once the running attempt was asked to stop, by
	// CancelNode or by a cancel of the pass.
	stopping bool
}

type finished struct {
	id     string
	result runner.Result
}

type startedProc struct {
	id      string
	attempt int
	pgid    int
}

type rerunRequest struct {
	id         string
	downstream bool
	start      store.Start
	reply      chan error
}

type cancelRequest struct {
	id    string
	reply chan cancelReply
}

type cancelReply struct {
	action plan.CancelAction
	err    error
}

// Engine runs one workflow.
type Engine struct {
	opts        Options
	concurrency int
	nodes       map[string]*nodeRun
	log         zerolog.Logger
	state       *store.State
	dirty       bool

	results  chan finished
	procs    chan startedProc
	requests chan rerunRequest
	cancels  chan cancelRequest
	runDone  chan struct{}

	closeOnce sync.Once
	closeCh   chan struct{}

	// mu guards the current pass's cancel state, which Interrupt, Cancel,
	// and Kill change from other goroutines.
	mu       sync.Mutex
	active   bool
	level    int
	cancelCh chan struct{}
	killCh   chan struct{}
}

// New returns an engine for opts.
func New(opts Options) *Engine {
	concurrency := opts.Workflow.Concurrency
	if opts.Concurrency > 0 {
		concurrency = opts.Concurrency
	}
	p := opts.Plan
	if p == nil {
		fresh := plan.Fresh(opts.Workflow)
		p = &fresh
	}
	nodes := make(map[string]*nodeRun, len(opts.Workflow.Nodes))
	for _, n := range opts.Workflow.Nodes {
		pn := p.Nodes[n.ID]
		nr := &nodeRun{
			node: n, state: pn.State, attempt: pn.Attempt,
			started: time.Time{}, finished: time.Time{}, exitCode: 0, reason: "", errText: "", log: "", pgid: 0,
			cancel: nil, kill: nil, stopping: false,
		}
		switch pn.State {
		case events.Skipped, events.Excluded:
			nr.started, nr.finished, nr.exitCode = pn.Prior.Started, pn.Prior.Finished, pn.Prior.ExitCode
			nr.log = pn.Prior.Log
			nr.reason = string(pn.State)
		default:
			nr.state = events.Pending
		}
		nodes[n.ID] = nr
	}
	return &Engine{
		opts:        opts,
		concurrency: concurrency,
		nodes:       nodes,
		log:         opts.Log,
		state:       opts.State,
		dirty:       opts.State != nil,
		results:     make(chan finished),
		procs:       make(chan startedProc),
		requests:    make(chan rerunRequest),
		cancels:     make(chan cancelRequest),
		runDone:     make(chan struct{}),
		closeOnce:   sync.Once{},
		closeCh:     make(chan struct{}),
		mu:          sync.Mutex{},
		// The first pass counts as active from the start, so a cancel that
		// arrives before Run still cancels it.
		active:   true,
		level:    0,
		cancelCh: make(chan struct{}),
		killCh:   make(chan struct{}),
	}
}

// Interrupt escalates a cancel of the current pass: the first call cancels
// it, later calls kill the running nodes. It returns the escalation level,
// or 0 when no pass is active, so callers can tell a request to stop the
// run from one that arrives after it ended.
func (e *Engine) Interrupt() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.active {
		return 0
	}
	e.level++
	e.escalate()
	return e.level
}

// Cancel stops new nodes from starting and starts graceful shutdown of the
// running ones. It does nothing between passes or once the pass is already
// canceling. It is safe to call from any goroutine.
func (e *Engine) Cancel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active && e.level == 0 {
		e.level = 1
		e.escalate()
	}
}

// Kill cancels the pass and skips the remaining grace periods: running
// nodes get SIGKILL at once.
func (e *Engine) Kill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active {
		e.level = max(e.level, 2)
		e.escalate()
	}
}

// escalate closes the channels for the current level. e.mu must be held.
func (e *Engine) escalate() {
	if e.level >= 1 && !isClosed(e.cancelCh) {
		close(e.cancelCh)
	}
	if e.level >= 2 && !isClosed(e.killCh) {
		close(e.killCh)
	}
}

// passChannels returns the current pass's cancel and kill channels.
func (e *Engine) passChannels() (cancel, kill <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelCh, e.killCh
}

// Active reports whether a pass is running or about to start. Between
// passes of an engine with Options.Reruns it is false.
func (e *Engine) Active() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

// cancelRequested reports whether the current pass has been asked to
// stop, even if the loop has not acted on it yet.
func (e *Engine) cancelRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.level > 0
}

func (e *Engine) setActive(active bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active = active
	if !active {
		e.level = 0
		e.cancelCh = make(chan struct{})
		e.killCh = make(chan struct{})
	}
}

// Close stops the engine taking reruns. Run returns once the current pass,
// if any, ends.
func (e *Engine) Close() {
	e.closeOnce.Do(func() { close(e.closeCh) })
}

// Rerun resets id, and with downstream every finished node downstream of
// it, to run again as a new attempt. plan.Rerun decides which nodes are
// eligible. During a pass the nodes join it; between passes they start a
// new one. start is recorded in the run's history; its Mode and Nodes are
// filled in here. Rerun returns ErrClosed once Run has returned and
// ErrCanceling while the pass is being canceled.
func (e *Engine) Rerun(id string, downstream bool, start store.Start) error {
	start.Mode, start.Nodes = store.ModeRerun, []string{id}
	req := rerunRequest{id: id, downstream: downstream, start: start, reply: make(chan error, 1)}
	select {
	case e.requests <- req:
	case <-e.runDone:
		return ErrClosed
	}
	return <-req.reply
}

// CancelNode stops one node and leaves the rest of the pass running. A
// node that has not started is marked canceled; a running one gets the
// SIGINT, kill_timeout, SIGTERM, SIGKILL shutdown, and a second CancelNode
// of the same attempt sends SIGKILL at once. Nodes that need it then block
// as for any canceled need. plan.Cancel decides what applies; CancelNode
// returns what it did, or ErrClosed once Run has returned.
func (e *Engine) CancelNode(id string) (plan.CancelAction, error) {
	req := cancelRequest{id: id, reply: make(chan cancelReply, 1)}
	select {
	case e.cancels <- req:
	case <-e.runDone:
		return 0, ErrClosed
	}
	r := <-req.reply
	return r.action, r.err
}

// Run executes the workflow and returns when every node has reached a
// terminal state, or with Options.Reruns, once the engine is closed and
// idle. Canceling ctx cancels the current pass and stops reruns.
func (e *Engine) Run(ctx context.Context) Summary {
	defer close(e.runDone)
	wf := e.opts.Workflow
	e.log.Info().Str("workflow", wf.Name).Int("nodes", len(wf.Nodes)).Int("concurrency", e.concurrency).Msg("run started")
	pass := 1
	e.publish(events.RunStarted{Workflow: wf.Name, RunID: e.opts.Run.ID, RunDir: e.opts.Run.Dir, Pass: pass, At: time.Now()})
	for _, n := range wf.Nodes {
		e.publishNode(e.nodes[n.ID])
	}
	e.persist()

	for {
		e.runPass(ctx)
		success := e.success()
		e.setActive(false)
		e.persist()
		e.log.Info().Int("pass", pass).Bool("success", success).Interface("counts", e.summary().Counts()).Msg("run finished")
		e.publish(events.RunFinished{Success: success, At: time.Now()})
		if !e.opts.Reruns || !e.idle(ctx) {
			break
		}
		pass++
		e.log.Info().Int("pass", pass).Msg("run restarted for a rerun")
		e.publish(events.RunStarted{Workflow: wf.Name, RunID: e.opts.Run.ID, RunDir: e.opts.Run.Dir, Pass: pass, At: time.Now()})
	}
	return e.summary()
}

// idle waits between passes for a rerun that starts the next one. It
// returns false when the engine closes or ctx is canceled.
func (e *Engine) idle(ctx context.Context) bool {
	for {
		select {
		case req := <-e.requests:
			// The pass is marked active before the reply, so an Interrupt
			// that follows a successful Rerun cancels the new pass.
			e.setActive(true)
			err := e.applyRerun(req, false)
			if err != nil {
				e.setActive(false)
			}
			req.reply <- err
			if err == nil {
				e.persist()
				return true
			}
		case req := <-e.cancels:
			req.reply <- e.applyCancel(req.id)
		case <-e.closeCh:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// runPass schedules nodes until none is running and none can start.
func (e *Engine) runPass(ctx context.Context) {
	running := 0
	canceling := false
	ctxDone := ctx.Done()
	cancelCh, killCh := e.passChannels()

	for {
		// Checked before scheduling, not only in the select, so a cancel
		// that lands together with a result never lets a new node start.
		if !canceling && (ctx.Err() != nil || isClosed(cancelCh)) {
			canceling = true
			ctxDone, cancelCh = nil, nil
			e.log.Info().Int("running", running).Msg("cancel requested; sending SIGINT to running nodes")
			for _, nr := range e.ordered() {
				if nr.state == events.Running {
					nr.stopping = true
					nr.cancel()
				}
			}
		}
		if canceling {
			e.cancelUnstarted()
		} else {
			running += e.schedule(ctx, running)
		}
		e.persist()
		if running == 0 {
			return
		}

		select {
		case f := <-e.results:
			running--
			e.finish(f.id, f.result)
			if !canceling {
				e.log.Debug().Str("node", f.id).Int("running", running).Msg("slot freed")
			}
		case p := <-e.procs:
			if nr := e.nodes[p.id]; nr.state == events.Running && nr.attempt == p.attempt {
				nr.pgid = p.pgid
				e.dirty = true
			}
		case req := <-e.requests:
			req.reply <- e.applyRerun(req, canceling)
		case req := <-e.cancels:
			req.reply <- e.applyCancel(req.id)
		case <-ctxDone:
		case <-cancelCh:
		case <-killCh:
			killCh = nil
			e.log.Warn().Int("running", running).Msg("kill requested; sending SIGKILL to running nodes")
			for _, nr := range e.ordered() {
				if nr.state == events.Running {
					killNode(nr)
				}
			}
		}
	}
}

// applyRerun resets the nodes of a rerun request.
func (e *Engine) applyRerun(req rerunRequest, canceling bool) error {
	if canceling || e.cancelRequested() {
		return ErrCanceling
	}
	states := make(map[string]events.State, len(e.nodes))
	for id, nr := range e.nodes {
		states[id] = nr.state
	}
	ids, err := plan.Rerun(e.opts.Workflow, states, req.id, req.downstream)
	if err != nil {
		e.log.Info().Str("node", req.id).Bool("downstream", req.downstream).Err(err).Msg("rerun refused")
		return err
	}
	e.log.Info().Str("node", req.id).Bool("downstream", req.downstream).Strs("nodes", ids).Msg("rerun requested")
	for _, id := range ids {
		nr := e.nodes[id]
		nr.state = events.Pending
		nr.attempt++
		nr.started, nr.finished = time.Time{}, time.Time{}
		nr.exitCode, nr.reason, nr.errText, nr.log, nr.pgid = 0, "", "", "", 0
		e.publishNode(nr)
	}
	if e.state != nil {
		e.state.History = append(e.state.History, req.start)
	}
	return nil
}

// applyCancel stops the node of a CancelNode request. The next scheduling
// step blocks whatever needs a node it canceled before start.
func (e *Engine) applyCancel(id string) cancelReply {
	states := make(map[string]events.State, len(e.nodes))
	for nid, nr := range e.nodes {
		states[nid] = nr.state
	}
	stopping := false
	if nr := e.nodes[id]; nr != nil {
		stopping = nr.stopping
	}
	action, err := plan.Cancel(e.opts.Workflow, states, id, stopping)
	if err != nil {
		e.log.Info().Str("node", id).Err(err).Msg("node cancel refused")
		return cancelReply{action: 0, err: err}
	}
	nr := e.nodes[id]
	log := e.nodeLog(nr)
	log.Info().Str("state", string(nr.state)).Stringer("action", action).Msg("node cancel requested")
	switch action {
	case plan.CancelUnstarted:
		e.cancelBeforeStart(nr)
	case plan.CancelRunning:
		nr.stopping = true
		nr.cancel()
	case plan.KillRunning:
		killNode(nr)
	}
	return cancelReply{action: action, err: nil}
}

// killNode skips the rest of a running node's grace period. A node can be
// killed by CancelNode and by a kill of the pass, so this runs more than
// once per attempt.
func killNode(nr *nodeRun) {
	if !isClosed(nr.kill) {
		close(nr.kill)
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (e *Engine) summary() Summary {
	s := Summary{Success: e.success(), States: make(map[string]events.State, len(e.nodes))}
	for id, nr := range e.nodes {
		s.States[id] = nr.state
	}
	return s
}

// success reports whether every node's state leaves the run successful.
// Excluded nodes were not asked to run, so they don't fail it.
func (e *Engine) success() bool {
	for _, nr := range e.nodes {
		switch nr.state {
		case events.Succeeded, events.Skipped, events.Excluded:
		case events.Failed:
			if !nr.node.ContinueOnError {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (e *Engine) ordered() []*nodeRun {
	out := make([]*nodeRun, 0, len(e.opts.Workflow.Nodes))
	for _, n := range e.opts.Workflow.Nodes {
		out = append(out, e.nodes[n.ID])
	}
	return out
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
				if plan.Blocks(need.node, need.state) {
					e.log.Debug().Str("node", nr.node.ID).Str("need", id).Str("need_state", string(need.state)).Msg("node blocked")
					e.block(nr, fmt.Sprintf("%s %s", id, need.state))
					changed = true
					ready = false
					break
				}
				if !plan.Satisfies(need.node, need.state) {
					ready = false
				}
			}
			if ready {
				e.log.Debug().Str("node", nr.node.ID).Strs("needs", nr.node.Needs).Msg("node ready: needs satisfied")
				nr.state = events.Ready
				e.publishNode(nr)
				changed = true
			}
		}
	}
}

// schedule resolves states and starts whatever may start now. It returns how
// many nodes it started.
func (e *Engine) schedule(ctx context.Context, running int) int {
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
				e.publishNode(nr)
			}
			return 0
		}
		e.start(ctx, nr)
		return 1
	}

	started := 0
	for _, nr := range ready {
		if running+started >= e.concurrency {
			e.log.Debug().Str("node", nr.node.ID).Int("running", running+started).Int("concurrency", e.concurrency).Msg("node ready but no free slot")
			break
		}
		e.start(ctx, nr)
		started++
	}
	return started
}

func (e *Engine) cancelUnstarted() {
	for _, nr := range e.ordered() {
		switch nr.state {
		case events.Pending, events.Ready, events.Waiting:
			e.cancelBeforeStart(nr)
		default:
		}
	}
}

func (e *Engine) cancelBeforeStart(nr *nodeRun) {
	e.log.Debug().Str("node", nr.node.ID).Str("from", string(nr.state)).Msg("node canceled before start")
	nr.state = events.Canceled
	nr.finished = time.Now()
	e.end(nr, runner.Result{ExitCode: -1, Signal: "", Reason: runner.ReasonCanceled, Err: nil}, "canceled before start", "")
}

func (e *Engine) block(nr *nodeRun, why string) {
	nr.state = events.Blocked
	nr.finished = time.Now()
	e.end(nr, runner.Result{ExitCode: -1, Signal: "", Reason: "blocked", Err: nil}, "blocked: "+why, "")
}

func (e *Engine) nodeLog(nr *nodeRun) zerolog.Logger {
	return e.log.With().Str("node", nr.node.ID).Int("attempt", nr.attempt).Logger()
}

func (e *Engine) start(ctx context.Context, nr *nodeRun) {
	log := e.nodeLog(nr)
	// Node contexts ignore the run context's cancellation: the main loop
	// cancels each node itself so that canceling, killing, and marking
	// unstarted nodes happen in one place and in order.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	nr.cancel = cancel
	nr.kill = make(chan struct{})
	nr.stopping = false
	nr.state = events.Running
	nr.started = time.Now()
	nr.log = e.opts.Run.RelPath(e.opts.Run.AttemptLogPath(nr.node.ID, nr.attempt))
	e.publishNode(nr)

	id, attempt := nr.node.ID, nr.attempt
	spec := runner.Spec{
		Node:        id,
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
		Started: func(pgid int) {
			select {
			case e.procs <- startedProc{id: id, attempt: attempt, pgid: pgid}:
			case <-ctx.Done():
			}
		},
		Log: log,
	}
	log.Info().Str("dir", spec.Dir).Bool("interactive", spec.Interactive).Msg("node started")

	results := e.results
	logFile, err := e.opts.Run.CreateAttemptLog(id, attempt)
	if err != nil {
		cancel()
		res := runner.Result{ExitCode: -1, Signal: "", Reason: runner.ReasonStartErr, Err: fmt.Errorf("create log file: %w", err)}
		go func() { results <- finished{id: id, result: res} }()
		return
	}

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
	env := make([]string, 0, len(e.opts.Env)+len(nr.node.Env)+6)
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
	nr.pgid = 0
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

	errText := ""
	if nr.state != events.Succeeded {
		errText = describe(res)
	}
	e.end(nr, res, errText, nr.log)
}

// end records a node's terminal state: the attempt record on disk, then
// the event.
func (e *Engine) end(nr *nodeRun, res runner.Result, errText, logPath string) {
	nr.exitCode, nr.reason, nr.errText, nr.log = res.ExitCode, string(res.Reason), errText, logPath
	e.writeRecord(nr, res)
	e.publishNode(nr)
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

func (e *Engine) writeRecord(nr *nodeRun, res runner.Result) {
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
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
		Log:         nr.log,
	}
	if err := e.opts.Run.WriteAttemptRecord(rec); err != nil {
		log := e.nodeLog(nr)
		log.Error().Err(err).Msg("write attempt record")
	}
}

func (e *Engine) publishNode(nr *nodeRun) {
	e.dirty = true
	exitCode := 0
	if nr.state.Terminal() {
		exitCode = nr.exitCode
	}
	e.publish(events.NodeChanged{
		Node:     nr.node.ID,
		State:    nr.state,
		Attempt:  nr.attempt,
		ExitCode: exitCode,
		Err:      nr.errText,
		Reason:   nr.reason,
		Started:  nr.started,
		Finished: nr.finished,
		At:       time.Now(),
	})
}

// persist writes state.json when a node changed since the last write. The
// engine loop calls it before it waits for anything, so a weft killed at
// any point leaves state.json at most one scheduling step behind, and
// never ahead of what happened.
func (e *Engine) persist() {
	if e.state == nil || !e.dirty {
		return
	}
	for id, nr := range e.nodes {
		e.state.Nodes[id] = store.NodeState{
			State: nr.state, Attempt: nr.attempt, ExitCode: nr.exitCode, Reason: nr.reason, Error: nr.errText,
			Started: nr.started, Finished: nr.finished, Log: nr.log, PGID: nr.pgid,
		}
	}
	e.state.Updated = time.Now()
	if err := e.opts.Run.WriteState(e.state); err != nil {
		e.log.Error().Err(err).Msg("write state; a resume of this run may rerun nodes that already succeeded")
		return
	}
	e.dirty = false
}

func (e *Engine) publish(ev events.Event) {
	if e.opts.Bus != nil {
		e.opts.Bus.Publish(ev)
	}
}
