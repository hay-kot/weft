package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/term"
	"github.com/hay-kot/weft/internal/ui/plain"
	"github.com/hay-kot/weft/internal/ui/tui"
	"github.com/hay-kot/weft/internal/workflow"
)

// UI modes for --ui.
const (
	uiAuto  = "auto"
	uiTUI   = "tui"
	uiPlain = "plain"
)

// RunCmd implements the run command.
type RunCmd struct {
	flags       *Flags
	ui          string
	concurrency int
	exitOnEnd   bool
	sel         selection
}

// NewRunCmd creates a new run command.
func NewRunCmd(flags *Flags) *RunCmd {
	return &RunCmd{
		flags: flags, ui: uiAuto, concurrency: 0, exitOnEnd: false,
		sel: selection{resume: false, from: "", only: nil, force: false},
	}
}

// Register adds the run command to the application.
func (cmd *RunCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "run",
		Usage:     "run a workflow",
		ArgsUsage: "<workflow>",
		Description: `Runs the nodes of a workflow as a DAG. <workflow> is a name such as
"deploy", which loads .weft/deploy.yml from the nearest directory that holds
a .weft/ directory, or a path to a workflow file.

With a terminal on stdin and stdout, and CI unset, weft shows a live graph
of the run with the selected node's log under it (--ui=tui). Otherwise it
prints one prefixed line per output line and state change (--ui=plain).
In the TUI, hjkl moves the selection, enter opens the full log, and ? lists
every key. The TUI stays open when the run ends until you press q, unless
--exit-on-end is set. weft exits non-zero when the run fails either way.

Each run gets an ID and a directory under .weft/runs/<workflow>/<run-id>/
that holds weft.log, events.jsonl, and per-node output logs. The TUI shows
both in its header; weft prints them to stderr at exit (and at start in
plain mode).

Ctrl-C cancels the run: running nodes get SIGINT and their kill_timeout
grace period, then SIGTERM, then SIGKILL. A second Ctrl-C sends SIGKILL at
once. SIGTERM and SIGHUP cancel the same way. In the TUI, x cancels only
the selected node, the same way, and a second x kills it. Nodes that need
it are blocked, and the rest of the run goes on.

--resume carries on the latest run in its own directory: nodes that
succeeded are skipped, and the rest run again as a new attempt. --from
<node> runs that node and everything downstream of it, and --only a,b runs
exactly those nodes. Both take the results of needs they leave out from the
latest run, and fail when those needs did not succeed there. A selection
that needs nothing from the latest run starts a new run instead, where the
nodes it leaves out show as excluded.

When the workflow file or git HEAD changed since the run being continued
last started, weft asks first in the TUI and refuses in plain mode unless
--force is set: resuming a release after HEAD moved is how the wrong commit
ships. In the TUI, r reruns the selected node and R reruns it with
everything downstream, during the run or after it ends.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "ui",
				Usage:       "output mode: auto, tui, or plain (auto picks tui when stdin and stdout are terminals and CI is unset)",
				Value:       uiAuto,
				Destination: &cmd.ui,
				Validator: func(s string) error {
					switch s {
					case uiAuto, uiTUI, uiPlain:
						return nil
					default:
						return fmt.Errorf("--ui must be auto, tui, or plain, got %q", s)
					}
				},
			},
			&cli.BoolFlag{
				Name:        "exit-on-end",
				Usage:       "quit the TUI as soon as the run ends instead of waiting for q",
				Destination: &cmd.exitOnEnd,
			},
			&cli.BoolFlag{
				Name:        "resume",
				Usage:       "carry on the latest run: skip nodes that succeeded and run the rest again",
				Destination: &cmd.sel.resume,
			},
			&cli.StringFlag{
				Name:        "from",
				Usage:       "run `NODE` and everything downstream of it, taking its needs from the latest run",
				Destination: &cmd.sel.from,
			},
			&cli.StringSliceFlag{
				Name:        "only",
				Usage:       "run only these `NODES` (comma-separated or repeated), taking needs they leave out from the latest run",
				Destination: &cmd.sel.only,
			},
			&cli.BoolFlag{
				Name:        "force",
				Usage:       "with --resume, --from, or --only: go ahead even if the workflow file or git HEAD changed since the run last started",
				Destination: &cmd.sel.force,
			},
			&cli.IntFlag{
				Name:        "concurrency",
				Aliases:     []string{"j"},
				Usage:       "maximum nodes running at once (default: the workflow's concurrency)",
				Destination: &cmd.concurrency,
				Validator: func(n int) error {
					if n < 0 {
						return errors.New("--concurrency must be positive")
					}
					return nil
				},
			},
		},
		Action: cmd.run,
	})

	return app
}

func (cmd *RunCmd) run(ctx context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return errors.New("usage: weft run <workflow>")
	}
	if err := cmd.sel.validate(); err != nil {
		return err
	}
	wf, err := loadWorkflow(c.Args().First())
	if err != nil {
		return err
	}
	mode, err := pickUI(cmd.ui, term.IsTerminal(os.Stdin), term.IsTerminal(os.Stdout), os.Getenv)
	if err != nil {
		return err
	}

	prep, err := prepare(ctx, wf, cmd.sel, time.Now())
	if errors.Is(err, errNothingToRun) {
		_, _ = fmt.Fprintf(os.Stderr, "weft: %v; nothing to resume. Start a new run without --resume.\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	defer prep.lock.Release()
	run := prep.run
	if len(prep.alive) > 0 && !cmd.sel.force {
		return aliveError(cmd.sel, run.ID, prep.alive)
	}
	confirm := ""
	if len(prep.drift) > 0 && !cmd.sel.force {
		if mode != uiTUI {
			return driftError(cmd.sel, run.ID, prep.drift)
		}
		// The header shows the run ID, so the question leaves it out and
		// keeps the answer keys on screen.
		confirm = fmt.Sprintf("Since this run last started, %s. y goes ahead, any other key quits.",
			strings.Join(prep.drift, " and "))
	}

	logFile, err := run.OpenLog()
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	// Nothing the engine waits on may write to stdout or stderr directly:
	// either can be a paused pipe, and a blocked write must never delay a
	// cancel. Renderers read from their own bus queue, and notices and
	// console logs go through an async writer.
	stderr := newAsyncWriter(os.Stderr, 256)
	// In the TUI, a log line on stderr would draw over the screen.
	var console io.Writer = stderr
	if mode == uiTUI {
		console = nil
	}
	runLog, err := cmd.runLogger(logFile, console, run)
	if err != nil {
		return err
	}
	runLog.Info().Str("ui", mode).Str("mode", prep.plan.Mode).Strs("selection", prep.plan.Selection).
		Bool("continue", prep.plan.Continue).Strs("pending", prep.plan.Pending(wf)).Strs("drift", prep.drift).
		Bool("force", cmd.sel.force).Str("git_head", prep.head).Msg("run planned")

	bus := events.NewBus()
	var eventLog sync.WaitGroup
	eventSub := bus.Subscribe(0)
	eventLog.Go(func() {
		if err := run.WriteEvents(eventSub.C); err != nil {
			runLog.Error().Err(err).Msg("write event log")
		}
	})

	r := &runSession{
		cmd: cmd, wf: wf, run: run, prep: prep, bus: bus, log: runLog, stderr: stderr,
		intr:   &interrupter{eng: nil},
		notice: startNotice(wf, prep.plan, run.ID, cmd.sel),
		mu:     sync.Mutex{}, baseline: prep.state.Last(),
	}
	var summary engine.Summary
	var uiErr error
	var abort <-chan struct{}
	started := true
	if mode == uiTUI {
		summary, abort, started, uiErr = r.tui(ctx, confirm)
	} else {
		summary, abort = r.plain(ctx, &eventLog)
	}
	eventLog.Wait()

	if !started {
		_, _ = fmt.Fprintf(stderr, "weft: run %s left as it was\nweft: run dir %s\n", run.ID, run.Dir)
		stderr.Close(noticeFlushTimeout, abort)
		if uiErr != nil {
			return fmt.Errorf("the TUI failed before the run started: %w", uiErr)
		}
		return driftError(cmd.sel, run.ID, prep.drift)
	}

	counts := summarize(summary)
	outcome := "succeeded"
	if !summary.Success {
		outcome = "failed"
	}
	_, _ = fmt.Fprintf(stderr, "weft: run %s %s (%s)\nweft: run dir %s\n", run.ID, outcome, counts, run.Dir)
	stderr.Close(noticeFlushTimeout, abort)
	if uiErr != nil {
		return fmt.Errorf("the TUI failed: %w; run %s %s (%s), logs in %s", uiErr, run.ID, outcome, counts, run.Dir)
	}
	if !summary.Success {
		return fmt.Errorf("run %s failed (%s); logs in %s", run.ID, counts, run.Dir)
	}
	return nil
}

// pickUI resolves --ui against the environment. auto picks the TUI only for
// an interactive terminal: both stdin and stdout are terminals, CI is
// unset, and TERM is not dumb.
func pickUI(flag string, stdinTTY, stdoutTTY bool, getenv func(string) string) (string, error) {
	switch flag {
	case uiPlain:
		return uiPlain, nil
	case uiTUI:
		if !stdinTTY || !stdoutTTY {
			return "", errors.New("--ui=tui needs a terminal on both stdin and stdout; use --ui=plain for pipes and CI")
		}
		return uiTUI, nil
	default:
		if stdinTTY && stdoutTTY && getenv("CI") == "" && getenv("TERM") != "dumb" {
			return uiTUI, nil
		}
		return uiPlain, nil
	}
}

// runSession holds what both output modes need for one run.
type runSession struct {
	cmd    *RunCmd
	wf     *workflow.Workflow
	run    *store.Run
	prep   *prepared
	bus    *events.Bus
	log    zerolog.Logger
	stderr *asyncWriter
	intr   *interrupter
	// notice describes a resumed or narrowed run, or is empty.
	notice string
	eng    *engine.Engine

	// mu guards baseline: what the workflow file and git HEAD were at the
	// start the user last accepted, which TUI reruns check against.
	mu       sync.Mutex
	baseline store.Start
}

func (r *runSession) engine(lender engine.TerminalLender, tty, reruns bool) *engine.Engine {
	p := r.prep.plan
	eng := engine.New(engine.Options{
		Workflow:    r.wf,
		Run:         r.run,
		Runner:      runner.Process{},
		Lender:      lender,
		Bus:         r.bus,
		Concurrency: r.cmd.concurrency,
		Env:         nodeEnv(tty),
		Plan:        &p,
		State:       r.prep.state,
		Reruns:      reruns,
		Log:         r.log,
	})
	r.intr.eng = eng
	r.eng = eng
	return eng
}

// begin records attempts the earlier weft lost track of, right before the
// engine starts. It only logs a failure: the records are for reading
// afterwards, and the run itself does not depend on them.
func (r *runSession) begin() {
	if lost := r.prep.alive; len(lost) > 0 {
		r.log.Warn().Int("alive", len(lost)).Msg("going ahead with --force although lost attempts may still be running")
	}
	if err := r.prep.recordLost(r.wf); err != nil {
		r.log.Error().Err(err).Msg("record lost attempts")
	}
}

// plain runs with the prefixed-line renderer on stdout.
func (r *runSession) plain(ctx context.Context, eventLog *sync.WaitGroup) (engine.Summary, <-chan struct{}) {
	stdoutTTY := term.IsTerminal(os.Stdout)
	color := stdoutTTY && !r.cmd.flags.NoColor
	_, _ = fmt.Fprintf(r.stderr, "weft: run %s\nweft: run dir %s\n", r.run.ID, r.run.Dir)
	if r.notice != "" {
		_, _ = fmt.Fprintf(r.stderr, "weft: %s\n", r.notice)
	}
	if r.cmd.sel.force && len(r.prep.drift) > 0 {
		_, _ = fmt.Fprintf(r.stderr, "weft: going ahead with --force although %s\n", strings.Join(r.prep.drift, " and "))
	}

	renderer := plain.New(os.Stdout, r.wf, color)
	renderSub := r.bus.Subscribe(0)
	var rendering sync.WaitGroup
	rendering.Go(func() { renderer.Run(renderSub.C) })

	eng := r.engine(plain.Lender{Sub: renderSub}, stdoutTTY, false)
	sigs := handleSignals(r.intr.Interrupt, r.log, func(level int, _ os.Signal) {
		if level == 1 {
			_, _ = fmt.Fprintln(r.stderr, "weft: canceling; running nodes get SIGINT and a grace period. Press Ctrl-C again to kill them now.")
			return
		}
		_, _ = fmt.Fprintln(r.stderr, "weft: killing running nodes")
	}, nil)
	defer sigs.stop()
	r.begin()
	summary := eng.Run(ctx)
	r.bus.Close()

	// Let the renderer and the event log catch up. With stdout paused that
	// could take forever, so give up once the renderer stops making
	// progress, or at once on a signal. The node logs on disk are complete
	// either way.
	drained := make(chan struct{})
	go func() { rendering.Wait(); eventLog.Wait(); close(drained) }()
	if !waitForOutput(drained, renderer.Rendered, outputStallTimeout, sigs.abort) {
		r.log.Warn().Msg("stdout is not being read; exiting without flushing the rest of the output")
	}
	return summary, sigs.abort
}

// tui runs with the terminal UI. It returns once the user quits and the run
// is over. If the TUI fails first, the run is canceled and weft waits for
// it with notices on stderr, where Ctrl-C works as in plain mode. With a
// confirm prompt, the engine starts only once the user accepts it; started
// reports whether it did.
func (r *runSession) tui(ctx context.Context, confirm string) (summary engine.Summary, abort <-chan struct{}, started bool, uiErr error) {
	start := make(chan struct{})
	var startOnce sync.Once
	p := r.prep.plan
	app := tui.NewApp(tui.Options{
		Workflow:  r.wf,
		Run:       r.run,
		ExitOnEnd: r.cmd.exitOnEnd,
		Interrupt: r.intr.Interrupt,
		Plan:      &p,
		Confirm:   confirm,
		Start:     func() { startOnce.Do(func() { close(start) }) },
		Rerun:     r.rerun,
		// The engine is created after the TUI, which lends it the terminal.
		CancelNode: func(id string) (plan.CancelAction, error) { return r.eng.CancelNode(id) },
		Notice:     r.notice,
		Log:        r.log,
	}, r.bus.Subscribe(0), r.cmd.flags.NoColor)
	eng := r.engine(app.Lender(), true, true)
	if confirm == "" {
		startOnce.Do(func() { close(start) })
	}

	var tuiGone atomic.Bool
	sigs := handleSignals(r.intr.Interrupt, r.log, func(level int, sig os.Signal) {
		if !tuiGone.Load() {
			app.Interrupted(level, sig.String())
			return
		}
		if level >= 2 {
			_, _ = fmt.Fprintln(r.stderr, "weft: killing running nodes")
		}
	}, app.Quit)
	defer sigs.stop()

	appDone := make(chan struct{})
	result := make(chan engine.Summary, 1)
	ran := make(chan bool, 1)
	go func() {
		select {
		case <-start:
		case <-appDone:
			// The TUI may have accepted just before it quit.
			select {
			case <-start:
			default:
				ran <- false
				r.bus.Close()
				return
			}
		}
		ran <- true
		r.begin()
		s := eng.Run(ctx)
		r.bus.Close()
		result <- s
	}()

	uiErr = app.Run()
	tuiGone.Store(true)
	close(appDone)
	if !<-ran {
		return engine.Summary{Success: false, States: nil}, sigs.abort, false, uiErr
	}
	// Checked before Close: between passes the run is over, and Run
	// returns as soon as the engine closes.
	midRun := eng.Active()
	// Reruns end with the TUI; Run returns once the current pass is over.
	eng.Close()
	if !midRun {
		return <-result, sigs.abort, true, uiErr
	}
	r.log.Error().AnErr("error", uiErr).Msg("TUI exited before the run finished; canceling the run")
	_, _ = fmt.Fprintf(r.stderr, "weft: the TUI exited before run %s finished; canceling it. Press Ctrl-C again to kill the running nodes now.\n", r.run.ID)
	if uiErr != nil {
		_, _ = fmt.Fprintf(r.stderr, "weft: TUI error: %v\n", uiErr)
	}
	r.intr.cancel()
	return <-result, sigs.abort, true, uiErr
}

// rerun handles r and R from the TUI. Unless force is set, it refuses with
// a *plan.DriftError when the workflow file or git HEAD changed since the
// start the user last accepted, so the TUI can ask first.
func (r *runSession) rerun(id string, downstream, force bool) error {
	head := gitHead(context.Background(), r.wf.Root)
	hash, err := workflow.HashFile(r.wf.Path)
	if err != nil {
		// A deleted or unreadable file counts as changed.
		hash = ""
	}
	r.mu.Lock()
	drift := plan.Drift(r.baseline, hash, head)
	r.mu.Unlock()
	if len(drift) > 0 && !force {
		return &plan.DriftError{RunID: r.run.ID, Reasons: drift}
	}
	start := store.Start{
		At: time.Now(), PID: os.Getpid(), Mode: store.ModeRerun, Nodes: nil,
		WorkflowHash: r.wf.Hash, GitHead: head, Forced: len(drift) > 0,
	}
	if err := r.eng.Rerun(id, downstream, start); err != nil {
		return err
	}
	if len(drift) > 0 {
		r.log.Warn().Strs("drift", drift).Str("node", id).Msg("rerun confirmed although the workflow or HEAD changed")
		r.mu.Lock()
		r.baseline = store.Start{
			At: start.At, PID: start.PID, Mode: store.ModeRerun, Nodes: nil,
			WorkflowHash: hash, GitHead: head, Forced: true,
		}
		r.mu.Unlock()
	}
	return nil
}

// interrupter forwards cancel requests from keys and signals to the
// engine, which escalates them per pass.
type interrupter struct {
	eng *engine.Engine
}

// cancel cancels the current pass without escalating to a kill.
func (i *interrupter) cancel() { i.eng.Cancel() }

// Interrupt escalates and returns the level: 1 cancels, 2 and up kill, and
// 0 means no pass is active.
func (i *interrupter) Interrupt() int { return i.eng.Interrupt() }

// outputStallTimeout is how long weft waits at exit for a renderer that has
// stopped making progress.
const outputStallTimeout = 3 * time.Second

// noticeFlushTimeout bounds the wait for weft's last stderr lines.
const noticeFlushTimeout = time.Second

// waitForOutput waits for done while progress keeps moving. It returns false
// when progress stalls for stall or abort closes first.
func waitForOutput(done <-chan struct{}, progress func() int64, stall time.Duration, abort <-chan struct{}) bool {
	tick := time.NewTicker(stall / 10)
	defer tick.Stop()
	last, since := progress(), time.Now()
	for {
		select {
		case <-done:
			return true
		case <-abort:
			return false
		case <-tick.C:
			if p := progress(); p != last {
				last, since = p, time.Now()
			} else if time.Since(since) >= stall {
				return false
			}
		}
	}
}

// runLogger returns a logger that writes JSON to the run's weft.log at the
// configured level and, when console is not nil, warnings and errors to it.
// Progress already reaches stderr through the renderer, so info lines there
// would repeat it.
func (cmd *RunCmd) runLogger(file, console io.Writer, run *store.Run) (zerolog.Logger, error) {
	level, err := zerolog.ParseLevel(cmd.flags.LogLevel)
	if err != nil {
		return zerolog.Nop(), fmt.Errorf("parse log level: %w", err)
	}
	writers := []io.Writer{levelWriter{w: file, min: level}}
	if console != nil {
		cw := zerolog.ConsoleWriter{Out: console, NoColor: cmd.flags.NoColor}
		writers = append(writers, levelWriter{w: cw, min: max(level, zerolog.WarnLevel)})
	}
	out := zerolog.MultiLevelWriter(writers...)
	l := zerolog.New(out).Level(level).With().Timestamp().Str("run_id", run.ID).Logger()
	log.Logger = l
	return l, nil
}

type levelWriter struct {
	w   io.Writer
	min zerolog.Level
}

func (lw levelWriter) Write(p []byte) (int, error) { return lw.w.Write(p) }

func (lw levelWriter) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	if l < lw.min {
		return len(p), nil
	}
	return lw.w.Write(p)
}

// nodeEnv returns the base environment for nodes. When weft renders to a
// terminal, tools that check isatty on their own pipe would drop color, so
// weft asks for it explicitly unless the user opted out with NO_COLOR.
func nodeEnv(tty bool) []string {
	env := os.Environ()
	if tty && os.Getenv("NO_COLOR") == "" {
		env = append(env, "CLICOLOR_FORCE=1", "FORCE_COLOR=1")
	}
	return env
}

type signalHandler struct {
	// abort closes on the first signal that arrives while no pass is
	// active, which means the run is over.
	abort    chan struct{}
	stopOnce sync.Once
	stop     func()
}

// handleSignals turns SIGINT, SIGTERM, and SIGHUP into cancel escalations
// and calls notify after each, so a stuck notice can't hold up the cancel.
// A signal while no pass is active closes abort and calls afterEnd, if
// set.
func handleSignals(interrupt func() int, l zerolog.Logger, notify func(level int, sig os.Signal), afterEnd func()) *signalHandler {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	h := &signalHandler{
		abort:    make(chan struct{}),
		stopOnce: sync.Once{},
		stop:     nil,
	}
	h.stop = func() {
		h.stopOnce.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
	go func() {
		for {
			select {
			case sig := <-ch:
				level := interrupt()
				switch level {
				case 0:
					l.Info().Str("signal", sig.String()).Msg("signal received after the run ended")
					close(h.abort)
					if afterEnd != nil {
						afterEnd()
					}
					return
				case 1:
					l.Info().Str("signal", sig.String()).Msg("signal received; canceling run")
				default:
					l.Warn().Str("signal", sig.String()).Msg("another signal received; killing nodes")
				}
				notify(level, sig)
			case <-done:
				return
			}
		}
	}()
	return h
}

func summarize(s engine.Summary) string {
	counts := s.Counts()
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, string(st))
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, st := range states {
		parts = append(parts, fmt.Sprintf("%d %s", counts[events.State(st)], st))
	}
	return strings.Join(parts, ", ")
}

func loadWorkflow(arg string) (*workflow.Workflow, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, root, err := workflow.Resolve(cwd, arg)
	if err != nil {
		return nil, err
	}
	return workflow.Load(path, root)
}
