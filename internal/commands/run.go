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
}

// NewRunCmd creates a new run command.
func NewRunCmd(flags *Flags) *RunCmd {
	return &RunCmd{flags: flags, ui: uiAuto, concurrency: 0, exitOnEnd: false}
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
once. SIGTERM and SIGHUP cancel the same way.`,
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
	wf, err := loadWorkflow(c.Args().First())
	if err != nil {
		return err
	}
	mode, err := pickUI(cmd.ui, term.IsTerminal(os.Stdin), term.IsTerminal(os.Stdout), os.Getenv)
	if err != nil {
		return err
	}

	// Prune before creating so the new run is one of the kept ones.
	if err := store.Prune(wf.Root, wf.Name, store.KeepRuns-1); err != nil {
		log.Warn().Err(err).Msg("prune old runs")
	}
	run, err := store.Create(wf.Root, wf.Name, time.Now())
	if err != nil {
		return err
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
	runLog.Info().Str("ui", mode).Msg("output mode")

	bus := events.NewBus()
	var eventLog sync.WaitGroup
	eventSub := bus.Subscribe(0)
	eventLog.Go(func() {
		if err := run.WriteEvents(eventSub.C); err != nil {
			runLog.Error().Err(err).Msg("write event log")
		}
	})

	r := runSession{
		cmd: cmd, wf: wf, run: run, bus: bus, log: runLog, stderr: stderr,
		intr: &interrupter{eng: nil, n: atomic.Int32{}},
	}
	var summary engine.Summary
	var uiErr error
	var abort <-chan struct{}
	if mode == uiTUI {
		summary, abort, uiErr = r.tui(ctx)
	} else {
		summary, abort = r.plain(ctx, &eventLog)
	}
	eventLog.Wait()

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
	bus    *events.Bus
	log    zerolog.Logger
	stderr *asyncWriter
	intr   *interrupter
}

func (r runSession) engine(lender engine.TerminalLender, tty bool) *engine.Engine {
	eng := engine.New(engine.Options{
		Workflow:    r.wf,
		Run:         r.run,
		Runner:      runner.Process{},
		Lender:      lender,
		Bus:         r.bus,
		Concurrency: r.cmd.concurrency,
		Env:         nodeEnv(tty),
		Log:         r.log,
	})
	r.intr.eng = eng
	return eng
}

// plain runs with the prefixed-line renderer on stdout.
func (r runSession) plain(ctx context.Context, eventLog *sync.WaitGroup) (engine.Summary, <-chan struct{}) {
	stdoutTTY := term.IsTerminal(os.Stdout)
	color := stdoutTTY && !r.cmd.flags.NoColor
	_, _ = fmt.Fprintf(r.stderr, "weft: run %s\nweft: run dir %s\n", r.run.ID, r.run.Dir)

	renderer := plain.New(os.Stdout, r.wf, color)
	renderSub := r.bus.Subscribe(0)
	var rendering sync.WaitGroup
	rendering.Go(func() { renderer.Run(renderSub.C) })

	eng := r.engine(plain.Lender{Sub: renderSub}, stdoutTTY)
	sigs := handleSignals(r.intr, r.log, func(level int, _ os.Signal) {
		if level == 1 {
			_, _ = fmt.Fprintln(r.stderr, "weft: canceling; running nodes get SIGINT and a grace period. Press Ctrl-C again to kill them now.")
			return
		}
		_, _ = fmt.Fprintln(r.stderr, "weft: killing running nodes")
	}, nil)
	defer sigs.stop()
	summary := eng.Run(ctx)
	sigs.runEnded()
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
// it with notices on stderr, where Ctrl-C works as in plain mode.
func (r runSession) tui(ctx context.Context) (engine.Summary, <-chan struct{}, error) {
	app := tui.NewApp(tui.Options{
		Workflow:  r.wf,
		Run:       r.run,
		ExitOnEnd: r.cmd.exitOnEnd,
		Interrupt: r.intr.Interrupt,
		Log:       r.log,
	}, r.bus.Subscribe(0), r.cmd.flags.NoColor)
	eng := r.engine(app.Lender(), true)

	var tuiGone atomic.Bool
	sigs := handleSignals(r.intr, r.log, func(level int, sig os.Signal) {
		if !tuiGone.Load() {
			app.Interrupted(level, sig.String())
			return
		}
		if level >= 2 {
			_, _ = fmt.Fprintln(r.stderr, "weft: killing running nodes")
		}
	}, app.Quit)
	defer sigs.stop()

	result := make(chan engine.Summary, 1)
	go func() {
		s := eng.Run(ctx)
		sigs.runEnded()
		r.bus.Close()
		result <- s
	}()

	uiErr := app.Run()
	tuiGone.Store(true)
	select {
	case s := <-result:
		return s, sigs.abort, uiErr
	default:
	}
	r.log.Error().AnErr("error", uiErr).Msg("TUI exited before the run finished; canceling the run")
	_, _ = fmt.Fprintf(r.stderr, "weft: the TUI exited before run %s finished; canceling it. Press Ctrl-C again to kill the running nodes now.\n", r.run.ID)
	if uiErr != nil {
		_, _ = fmt.Fprintf(r.stderr, "weft: TUI error: %v\n", uiErr)
	}
	r.intr.cancel()
	return <-result, sigs.abort, uiErr
}

// interrupter escalates cancel requests from keys and signals alike: the
// first cancels the run, every later one kills the running nodes.
type interrupter struct {
	eng *engine.Engine
	n   atomic.Int32
}

// cancel cancels the run unless an interrupt already has, without
// escalating to a kill.
func (i *interrupter) cancel() {
	if i.n.CompareAndSwap(0, 1) {
		i.eng.Cancel()
	}
}

// Interrupt escalates and returns how many interrupts there have been.
func (i *interrupter) Interrupt() int {
	n := int(i.n.Add(1))
	if n == 1 {
		i.eng.Cancel()
	} else {
		i.eng.Kill()
	}
	return n
}

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
	// abort closes on the first signal after the run ends.
	abort    chan struct{}
	ended    chan struct{}
	stopOnce sync.Once
	stop     func()
}

func (h *signalHandler) runEnded() { close(h.ended) }

// handleSignals turns SIGINT, SIGTERM, and SIGHUP into cancel escalations
// and calls notify after each, so a stuck notice can't hold up the cancel.
// A signal after the run ends closes abort and calls afterEnd, if set.
func handleSignals(intr *interrupter, l zerolog.Logger, notify func(level int, sig os.Signal), afterEnd func()) *signalHandler {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	h := &signalHandler{
		abort:    make(chan struct{}),
		ended:    make(chan struct{}),
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
				select {
				case <-h.ended:
					close(h.abort)
					if afterEnd != nil {
						afterEnd()
					}
					return
				default:
				}
				level := intr.Interrupt()
				if level == 1 {
					l.Info().Str("signal", sig.String()).Msg("signal received; canceling run")
				} else {
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
