package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/commands"
)

var (
	// Build information. Populated at build-time via -ldflags flag.
	version = "dev"
	commit  = "HEAD"
	date    = "now"
)

func build() string {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			version = info.Main.Version
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					commit = s.Value
				case "vcs.time":
					date = s.Value
				}
			}
		}
	}

	short := commit
	if len(commit) > 7 {
		short = commit[:7]
	}

	return fmt.Sprintf("%s (%s) %s", version, short, date)
}

// setupLogger sends weft's own log to stderr until a run replaces it with
// the run's weft.log.
func setupLogger(level string, noColor bool) error {
	parsedLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: noColor}).Level(parsedLevel)
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.DurationFieldUnit = time.Millisecond
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	flags := &commands.Flags{}

	app := &cli.Command{
		Name:  "weft",
		Usage: `Run task-runner tasks as a DAG with a live graph in your terminal`,
		Description: `A workflow is a YAML file in .weft/<name>.yml at the root of a project.
Each node runs a shell command once the nodes it needs have succeeded:

  # .weft/deploy.yml
  concurrency: 4
  nodes:
    check:
      run: mise run check
    release:
      needs: [check]
      run: mise run release
    apply:
      needs: [release]
      interactive: true    # gets the real terminal, for prompts
      run: tofu apply

Node fields: run (required), needs, label, dir, env, shell, interactive,
timeout, kill_timeout, continue_on_error. Top-level fields: concurrency,
shell, nodes. weft validate checks a file; weft run runs it.

Run "weft <command> --help" for each command, and see the README for every
field.`,
		Version:               build(),
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "log-level",
				Usage:       "log level (debug, info, warn, error, fatal, panic)",
				Sources:     cli.EnvVars("WEFT_LOG_LEVEL"),
				Value:       "info",
				Destination: &flags.LogLevel,
			},
			&cli.BoolFlag{
				Name:        "no-color",
				Usage:       "disable colored output",
				Sources:     cli.EnvVars("NO_COLOR"),
				Destination: &flags.NoColor,
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			return ctx, setupLogger(flags.LogLevel, flags.NoColor)
		},
	}
	app = commands.NewRunCmd(flags).Register(app)
	app = commands.NewValidateCmd(flags).Register(app)
	app = commands.NewGraphCmd(flags).Register(app)
	// +scaffold:command:register

	// Without a handler, Go exits on SIGPIPE when stdout or stderr is a
	// closed pipe, which would orphan running nodes mid-deploy. Handling it
	// (rather than ignoring it) turns such writes into EPIPE errors while
	// children still start with default SIGPIPE behavior.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args); err != nil {
		colorRed := "\033[38;2;215;95;107m"
		colorGray := "\033[38;2;163;163;163m"
		colorReset := "\033[0m"
		if flags.NoColor {
			colorRed = ""
			colorGray = ""
			colorReset = ""
		}
		msg := fmt.Sprintf("\n%s╭ Error%s\n%s│%s %s%s%s\n%s╵%s\n",
			colorRed, colorReset,
			colorRed, colorReset, colorGray, err.Error(), colorReset,
			colorRed, colorReset,
		)
		// stderr may be a paused pipe; exiting with the right status matters
		// more than this message.
		written := make(chan struct{})
		go func() {
			_, _ = os.Stderr.WriteString(msg)
			close(written)
		}()
		select {
		case <-written:
		case <-time.After(time.Second):
		}
		return 1
	}

	return 0
}
