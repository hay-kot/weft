package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/report"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// defaultTail is how many log lines `weft status` shows per node that did
// not succeed: enough for most error messages, small enough for an agent's
// context.
const defaultTail = 20

// runFlag selects a run by ID for status and logs.
func runFlag(dst *string) *cli.StringFlag {
	return &cli.StringFlag{
		Name:        "run",
		Usage:       "the run ID to read (default: the latest run; see weft runs)",
		Destination: dst,
	}
}

// workflowRuns finds a workflow and its run directories from a workflow
// argument. The workflow is loaded for node labels and order; when the
// file no longer parses, wf is nil and the runs are still readable.
type workflowRuns struct {
	arg  string
	name string
	root string
	wf   *workflow.Workflow
}

func findWorkflowRuns(arg string) (*workflowRuns, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, root, err := workflow.Resolve(cwd, arg)
	if err != nil {
		return nil, err
	}
	name, err := workflow.Name(path)
	if err != nil {
		return nil, err
	}
	w := &workflowRuns{arg: arg, name: name, root: root, wf: nil}
	if wf, err := workflow.Load(path, root); err == nil {
		w.wf = wf
	}
	return w, nil
}

// open returns the run with the given ID, or the latest run when id is
// empty.
func (w *workflowRuns) open(id string) (*store.Run, error) {
	if id != "" {
		return store.Open(w.root, w.name, id)
	}
	run, err := store.Latest(w.root, w.name)
	if errors.Is(err, store.ErrNoRuns) {
		return nil, fmt.Errorf("%s has no runs yet; start one with 'weft run %s'", w.name, w.arg)
	}
	return run, err
}

func (w *workflowRuns) report(run *store.Run) (report.Run, error) {
	st, err := run.LoadState()
	if errors.Is(err, store.ErrNoState) {
		return report.Run{}, fmt.Errorf("run %s has no state.json; it did not get as far as starting a node", run.ID) //nolint:exhaustruct // an error result
	}
	if err != nil {
		return report.Run{}, err //nolint:exhaustruct // an error result
	}
	return report.Build(run, st, w.wf), nil
}

// RunsCmd implements the runs command.
type RunsCmd struct {
	flags *Flags
	json  bool
	limit int
}

// NewRunsCmd creates a new runs command.
func NewRunsCmd(flags *Flags) *RunsCmd {
	return &RunsCmd{flags: flags, json: false, limit: 0}
}

// Register adds the runs command to the application.
func (cmd *RunsCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "runs",
		Usage:     "list a workflow's runs, newest first",
		ArgsUsage: "<workflow>",
		Description: `Lists the runs weft kept for a workflow (the newest 20), newest first:
run ID, status, start time, how it last started (run, resume, from, only, or
rerun), node counts, and the nodes that did not succeed.

A status is running while a weft process holds the run, interrupted when no
weft holds it but nodes never finished (the weft that ran it died), and
otherwise succeeded, failed, or canceled.

With --json, prints one JSON object per line (JSON Lines), the same shape as
weft status --json without the nodes.

Examples:
  weft runs deploy
  weft runs deploy --json --limit 1`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "json",
				Usage:       "print one JSON object per run per line",
				Destination: &cmd.json,
			},
			&cli.IntFlag{
				Name:        "limit",
				Usage:       "list at most this many runs (0 lists all)",
				Destination: &cmd.limit,
			},
		},
		Action:        cmd.run,
		ShellComplete: completeWorkflow,
	})
	return app
}

func (cmd *RunsCmd) run(_ context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return errors.New("usage: weft runs <workflow>")
	}
	w, err := findWorkflowRuns(c.Args().First())
	if err != nil {
		return err
	}
	runs, err := store.List(w.root, w.name)
	if err != nil {
		return err
	}
	if cmd.limit > 0 {
		runs = runs[:min(len(runs), cmd.limit)]
	}
	return writeRuns(c.Root().Writer, w, runs, cmd.json)
}

func writeRuns(out io.Writer, w *workflowRuns, runs []*store.Run, asJSON bool) error {
	if len(runs) == 0 && !asJSON {
		_, err := fmt.Fprintf(out, "%s has no runs yet; start one with 'weft run %s'\n", w.name, w.arg)
		return err
	}
	enc := json.NewEncoder(out)
	for _, run := range runs {
		r, err := w.report(run)
		if err != nil {
			// One unreadable run must not hide the others.
			if asJSON {
				err = enc.Encode(map[string]string{"workflow": w.name, "run_id": run.ID, "error": err.Error()})
			} else {
				_, err = fmt.Fprintf(out, "%s  unreadable: %v\n", run.ID, err)
			}
			if err != nil {
				return err
			}
			continue
		}
		if asJSON {
			if err := enc.Encode(r.Summary()); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(out, r.Line()); err != nil {
			return err
		}
	}
	return nil
}

// StatusCmd implements the status command.
type StatusCmd struct {
	flags *Flags
	json  bool
	run   string
	tail  int
}

// NewStatusCmd creates a new status command.
func NewStatusCmd(flags *Flags) *StatusCmd {
	return &StatusCmd{flags: flags, json: false, run: "", tail: defaultTail}
}

// Register adds the status command to the application.
func (cmd *StatusCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "status",
		Usage:     "show how a run went, node by node",
		ArgsUsage: "<workflow>",
		Description: `Shows the latest run of a workflow, or the one --run names: its status,
each node's state, attempt, duration, and error, and the last lines of the
log of every node that did not succeed. It ends with the commands to run
next, such as weft logs for a failed node's full output and weft run
--resume to carry the run on.

It reads only what weft wrote to the run directory, so it works during a
run, from another terminal, and long after.

With --json, prints one JSON object: workflow, run_id, run_dir, status,
created, updated, history (every start of the run), counts (nodes per
state), and nodes (id, label, state, attempt, exit_code, reason, error,
started, finished, duration_ms, log, and log_tail for nodes that did not
succeed).

Exits 0 when it could read the run, whatever the run's status.

Examples:
  weft status deploy
  weft status deploy --run 20261025T191200.482-a3f9 --tail 50
  weft status deploy --json | jq -r '.nodes[] | select(.state == "failed") | .log'`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "json",
				Usage:       "print the run as one JSON object",
				Destination: &cmd.json,
			},
			runFlag(&cmd.run),
			&cli.IntFlag{
				Name:        "tail",
				Usage:       "log lines to show for each node that did not succeed (0 for none)",
				Value:       defaultTail,
				Destination: &cmd.tail,
			},
		},
		Action:        cmd.runCmd,
		ShellComplete: completeWorkflow,
	})
	return app
}

func (cmd *StatusCmd) runCmd(_ context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return errors.New("usage: weft status <workflow>")
	}
	w, err := findWorkflowRuns(c.Args().First())
	if err != nil {
		return err
	}
	run, err := w.open(cmd.run)
	if err != nil {
		return err
	}
	r, err := w.report(run)
	if err != nil {
		return err
	}
	r.AddLogTails(cmd.tail)
	if cmd.json {
		return json.NewEncoder(c.Root().Writer).Encode(r)
	}
	latest, err := store.Latest(w.root, w.name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.Root().Writer, r.Text(w.arg, latest.ID == run.ID))
	return err
}

// LogsCmd implements the logs command.
type LogsCmd struct {
	flags   *Flags
	run     string
	attempt int
	tail    int
}

// NewLogsCmd creates a new logs command.
func NewLogsCmd(flags *Flags) *LogsCmd {
	return &LogsCmd{flags: flags, run: "", attempt: 0, tail: 0}
}

// Register adds the logs command to the application.
func (cmd *LogsCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "logs",
		Usage:     "print a node's output from a run",
		ArgsUsage: "<workflow> <node>",
		Description: `Prints the combined stdout and stderr of one node attempt, as weft saved it
under the run directory: the node's latest attempt in the latest run unless
--run or --attempt says otherwise. An interactive node's output went to the
terminal, so its log says only how it ended.

Examples:
  weft logs deploy release
  weft logs deploy release --attempt 1 --tail 100`,
		Flags: []cli.Flag{
			runFlag(&cmd.run),
			&cli.IntFlag{
				Name:        "attempt",
				Usage:       "the attempt to print (default: the node's latest attempt)",
				Destination: &cmd.attempt,
			},
			&cli.IntFlag{
				Name:        "tail",
				Usage:       "print only the last N lines (0 prints the whole log)",
				Destination: &cmd.tail,
			},
		},
		Action:        cmd.runCmd,
		ShellComplete: completeWorkflowThenNode,
	})
	return app
}

func (cmd *LogsCmd) runCmd(_ context.Context, c *cli.Command) error {
	if c.NArg() != 2 {
		return errors.New("usage: weft logs <workflow> <node>")
	}
	w, err := findWorkflowRuns(c.Args().First())
	if err != nil {
		return err
	}
	run, err := w.open(cmd.run)
	if err != nil {
		return err
	}
	path, err := attemptLog(run, c.Args().Get(1), cmd.attempt)
	if err != nil {
		return err
	}
	return copyLog(c.Root().Writer, path, cmd.tail)
}

// attemptLog returns the log path of a node's attempt, or of its latest
// attempt when attempt is zero.
func attemptLog(run *store.Run, node string, attempt int) (string, error) {
	st, err := run.LoadState()
	if err != nil {
		return "", err
	}
	ns, ok := st.Nodes[node]
	if !ok || !workflow.ValidID(node) {
		ids := slices.DeleteFunc(slices.Sorted(maps.Keys(st.Nodes)), func(id string) bool { return !workflow.ValidID(id) })
		return "", fmt.Errorf("run %s has no node %q; its nodes are %s", run.ID, node, strings.Join(ids, ", "))
	}
	if ns.Attempt == 0 {
		return "", fmt.Errorf("%s never ran in run %s (it is %s)", node, run.ID, ns.State)
	}
	if attempt == 0 {
		attempt = ns.Attempt
	}
	if attempt < 1 || attempt > ns.Attempt {
		return "", fmt.Errorf("%s has attempts 1 to %d in run %s, not %d", node, ns.Attempt, run.ID, attempt)
	}
	path := run.AttemptLogPath(node, attempt)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s attempt %d in run %s has no log (it is %s); see 'weft status'", node, attempt, run.ID, ns.State)
	}
	return path, nil
}

func copyLog(out io.Writer, path string, tail int) error {
	if tail > 0 {
		lines, err := report.Tail(path, tail)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := fmt.Fprintln(out, l); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := store.OpenRegular(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(out, f)
	return err
}
