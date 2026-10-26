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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/ui/plain"
	"github.com/hay-kot/weft/internal/workflow"
)

// UI modes for --ui.
const (
	uiAuto  = "auto"
	uiPlain = "plain"
)

// RunCmd implements the run command.
type RunCmd struct {
	flags       *Flags
	ui          string
	concurrency int
}

// NewRunCmd creates a new run command.
func NewRunCmd(flags *Flags) *RunCmd {
	return &RunCmd{flags: flags, ui: uiAuto, concurrency: 0}
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

Each run gets an ID and a directory under .weft/runs/<workflow>/<run-id>/
that holds weft.log, events.jsonl, and per-node output logs. weft prints
both at start and at exit.

Ctrl-C cancels the run: running nodes get SIGINT and their kill_timeout
grace period, then SIGTERM, then SIGKILL. A second Ctrl-C sends SIGKILL at
once.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "ui",
				Usage:       "output mode: auto or plain",
				Value:       uiAuto,
				Destination: &cmd.ui,
				Validator: func(s string) error {
					switch s {
					case uiAuto, uiPlain:
						return nil
					default:
						return fmt.Errorf("--ui must be auto or plain, got %q", s)
					}
				},
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
	// cancel. The renderer reads from its own bus queue, and notices and
	// console logs go through an async writer.
	stderr := newAsyncWriter(os.Stderr, 256)
	runLog, err := cmd.runLogger(logFile, stderr, run)
	if err != nil {
		return err
	}

	stdoutTTY := runner.IsTerminal(os.Stdout)
	color := stdoutTTY && !cmd.flags.NoColor
	_, _ = fmt.Fprintf(stderr, "weft: run %s\nweft: run dir %s\n", run.ID, run.Dir)

	bus := events.NewBus()
	var wg sync.WaitGroup
	eventSub := bus.Subscribe(0)
	wg.Go(func() {
		if err := run.WriteEvents(eventSub.C); err != nil {
			runLog.Error().Err(err).Msg("write event log")
		}
	})
	renderer := plain.New(os.Stdout, wf, color)
	renderSub := bus.Subscribe(0)
	wg.Go(func() { renderer.Run(renderSub.C) })

	eng := engine.New(engine.Options{
		Workflow:    wf,
		Run:         run,
		Runner:      runner.Process{},
		Lender:      plain.Lender{Sub: renderSub},
		Bus:         bus,
		Concurrency: cmd.concurrency,
		Env:         nodeEnv(stdoutTTY),
		Log:         runLog,
	})

	sigs := handleSignals(eng, stderr, runLog)
	defer sigs.stop()
	summary := eng.Run(ctx)
	sigs.runEnded()
	bus.Close()

	// Let the renderer and the event log catch up. With stdout paused that
	// could take forever, so give up once the renderer stops making
	// progress, or at once on a signal. The node logs on disk are complete
	// either way.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	if !waitForOutput(drained, renderer.Rendered, outputStallTimeout, sigs.abort) {
		runLog.Warn().Msg("stdout is not being read; exiting without flushing the rest of the output")
	}

	counts := summarize(summary)
	outcome := "succeeded"
	if !summary.Success {
		outcome = "failed"
	}
	_, _ = fmt.Fprintf(stderr, "weft: run %s %s (%s)\nweft: run dir %s\n", run.ID, outcome, counts, run.Dir)
	stderr.Close(noticeFlushTimeout, sigs.abort)
	if !summary.Success {
		return fmt.Errorf("run %s failed (%s); logs in %s", run.ID, counts, run.Dir)
	}
	return nil
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
// configured level and warnings and errors to stderr. Progress already
// reaches stderr through the renderer, so info lines there would repeat it.
func (cmd *RunCmd) runLogger(file, stderr io.Writer, run *store.Run) (zerolog.Logger, error) {
	level, err := zerolog.ParseLevel(cmd.flags.LogLevel)
	if err != nil {
		return zerolog.Nop(), fmt.Errorf("parse log level: %w", err)
	}
	console := zerolog.ConsoleWriter{Out: stderr, NoColor: cmd.flags.NoColor}
	out := zerolog.MultiLevelWriter(
		levelWriter{w: file, min: level},
		levelWriter{w: console, min: max(level, zerolog.WarnLevel)},
	)
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

// handleSignals turns the first SIGINT or SIGTERM into a graceful cancel
// and the second into a kill. A signal after the run ends closes abort. The
// engine is told first and the notice printed after, so a stuck stderr can't
// hold up the cancel.
func handleSignals(eng *engine.Engine, notices io.Writer, l zerolog.Logger) *signalHandler {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
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
		count := 0
		for {
			select {
			case sig := <-ch:
				select {
				case <-h.ended:
					close(h.abort)
					return
				default:
				}
				count++
				if count == 1 {
					eng.Cancel()
					l.Info().Str("signal", sig.String()).Msg("signal received; canceling run")
					_, _ = fmt.Fprintln(notices, "weft: canceling; running nodes get SIGINT and a grace period. Press Ctrl-C again to kill them now.")
					continue
				}
				eng.Kill()
				l.Warn().Str("signal", sig.String()).Msg("second signal received; killing nodes")
				_, _ = fmt.Fprintln(notices, "weft: killing running nodes")
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
