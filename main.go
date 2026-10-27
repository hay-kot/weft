package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/commands"
	"github.com/hay-kot/weft/internal/term"
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
		return fmt.Errorf("--log-level: %q is not a level; use debug, info, warn, error, fatal, or panic", level)
	}
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: noColor || !term.IsTerminal(os.Stderr)}).Level(parsedLevel)
	return nil
}

// noColorEnv follows no-color.org: NO_COLOR set to anything but the empty
// string turns color off. It is read here rather than as the flag's env
// source because urfave/cli parses a bool source strictly, and NO_COLOR=yes
// would make every weft command fail.
func noColorEnv(getenv func(string) string) bool {
	return getenv("NO_COLOR") != ""
}

// unknownCommand runs when no subcommand matched. Without it, urfave/cli
// answers a typo with "No help topic" and exit status 3.
func unknownCommand(_ context.Context, c *cli.Command) error {
	if c.NArg() == 0 {
		return cli.ShowRootCommandHelp(c)
	}
	name := c.Args().First()
	if s := cli.SuggestCommand(c.Commands, name); s != "" {
		return fmt.Errorf("unknown command %q; did you mean %q? Run 'weft --help' for the commands", name, s)
	}
	return fmt.Errorf("unknown command %q; run 'weft --help' for the commands", name)
}

const unknownFlagPrefix = "flag provided but not defined: -"

// usageError hands a flag error back as is. Without it, urfave/cli prints
// the error, then the command's whole help text, and run prints the error
// once more in its box. A misspelled flag still gets a suggestion.
func usageError(_ context.Context, c *cli.Command, err error, _ bool) error {
	if name, ok := strings.CutPrefix(err.Error(), unknownFlagPrefix); ok {
		flags := slices.Concat(c.Flags, c.Root().Flags)
		if s := cli.SuggestFlag(flags, name, false); s != "" {
			help := "weft --help"
			if c != c.Root() {
				help = "weft " + c.Name + " --help"
			}
			return fmt.Errorf("%w; did you mean %s? Run '%s' for the flags", err, s, help)
		}
	}
	return err
}

func main() {
	os.Exit(run())
}

// newApp builds the command tree. flags receives the global flags once the
// command line is parsed.
func newApp(flags *commands.Flags) *cli.Command {
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

After a run, weft status shows how it went, weft logs prints a node's
output, and weft runs lists earlier runs. Agents and scripts: weft doc
prints a compact reference, and status, runs, validate, and graph take
--json.

Run "weft <command> --help" for each command, and see the README for every
field.`,
		Suggest:               true,
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
				Usage:       "disable colored output (NO_COLOR in the environment does the same)",
				Destination: &flags.NoColor,
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if noColorEnv(os.Getenv) {
				flags.NoColor = true
			}
			return ctx, setupLogger(flags.LogLevel, flags.NoColor)
		},
		Action:       unknownCommand,
		OnUsageError: usageError,
	}
	app = commands.NewRunCmd(flags).Register(app)
	app = commands.NewValidateCmd(flags).Register(app)
	app = commands.NewGraphCmd(flags).Register(app)
	app = commands.NewRunsCmd(flags).Register(app)
	app = commands.NewStatusCmd(flags).Register(app)
	app = commands.NewLogsCmd(flags).Register(app)
	app = commands.NewDocCmd(flags).Register(app)
	// +scaffold:command:register
	for _, c := range app.Commands {
		c.OnUsageError = usageError
	}
	return app
}

func run() int {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.DurationFieldUnit = time.Millisecond
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	flags := &commands.Flags{}
	app := newApp(flags)

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
		// An agent or a log file reads stderr as text; escape codes are noise.
		if flags.NoColor || !term.IsTerminal(os.Stderr) {
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
