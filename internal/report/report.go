// Package report summarizes runs from what weft left on disk, for
// `weft runs` and `weft status`. The same types render as text for people
// and agents, and as JSON for scripts.
package report

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/ui"
	"github.com/hay-kot/weft/internal/workflow"
)

// Status is a run's overall outcome.
type Status string

// Run statuses.
const (
	// Running: a weft process holds the run's lock.
	Running Status = "running"
	// Succeeded: every node succeeded, was skipped, or was excluded.
	Succeeded Status = "succeeded"
	// Failed: a node failed, or was blocked by one that did.
	Failed Status = "failed"
	// Canceled: the run or a node was canceled, and nothing failed.
	Canceled Status = "canceled"
	// Interrupted: no weft holds the run, yet nodes never finished, so the
	// weft that ran it died. --resume carries it on.
	Interrupted Status = "interrupted"
)

// stateOrder is the order states appear in counts and progress lines.
var stateOrder = []events.State{
	events.Running, events.Waiting, events.Ready, events.Pending,
	events.Succeeded, events.Skipped, events.Failed, events.Blocked,
	events.Canceled, events.Excluded,
}

// Node is one node's latest attempt in a run.
type Node struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	State    events.State  `json:"state"`
	Attempt  int           `json:"attempt"`
	ExitCode int           `json:"exit_code"`
	Reason   events.Reason `json:"reason,omitempty"`
	Error    string        `json:"error,omitempty"`
	Started  time.Time     `json:"started,omitzero"`
	Finished time.Time     `json:"finished,omitzero"`
	// DurationMS is how long the attempt ran, or has run so far.
	DurationMS int64 `json:"duration_ms"`
	// Log is the absolute path of the attempt's output log.
	Log string `json:"log,omitempty"`
	// LogTail holds the last lines of the log of a node that did not
	// succeed, when asked for.
	LogTail []string `json:"log_tail,omitempty"`
}

// Run summarizes one run.
type Run struct {
	Workflow string `json:"workflow"`
	RunID    string `json:"run_id"`
	RunDir   string `json:"run_dir"`
	Status   Status `json:"status"`
	// PID is the weft process that holds a running run, when known.
	PID     int                  `json:"pid,omitempty"`
	Created time.Time            `json:"created"`
	Updated time.Time            `json:"updated"`
	History []store.Start        `json:"history"`
	Counts  map[events.State]int `json:"counts"`
	// Nodes follow the workflow file's order. A node the file no longer
	// has comes last.
	Nodes []Node `json:"nodes"`
}

// Summary is a run without its nodes, for one line of `weft runs --json`.
type Summary struct {
	Workflow string               `json:"workflow"`
	RunID    string               `json:"run_id"`
	RunDir   string               `json:"run_dir"`
	Status   Status               `json:"status"`
	PID      int                  `json:"pid,omitempty"`
	Created  time.Time            `json:"created"`
	Updated  time.Time            `json:"updated"`
	History  []store.Start        `json:"history"`
	Counts   map[events.State]int `json:"counts"`
	// Unfinished lists the nodes that did not succeed, skip, or get left
	// out, in node order.
	Unfinished []string `json:"unfinished"`
}

// Summary drops the nodes, keeping only the IDs of the unfinished ones.
func (r *Run) Summary() Summary {
	unfinished := []string{}
	for _, n := range r.Unfinished() {
		unfinished = append(unfinished, n.ID)
	}
	return Summary{
		Workflow: r.Workflow, RunID: r.RunID, RunDir: r.RunDir, Status: r.Status, PID: r.PID,
		Created: r.Created, Updated: r.Updated, History: r.History, Counts: r.Counts, Unfinished: unfinished,
	}
}

// Build summarizes a run from its state. wf may be nil when the workflow
// file is gone; nodes then sort by ID and have no labels.
func Build(run *store.Run, st *store.State, wf *workflow.Workflow) Run {
	pid, inUse := run.InUse()
	if !inUse {
		pid = 0
	}
	r := Run{
		Workflow: run.Workflow, RunID: run.ID, RunDir: run.Dir, Status: "", PID: pid,
		Created: st.Created, Updated: st.Updated, History: st.History,
		Counts: map[events.State]int{}, Nodes: nil,
	}
	if r.History == nil {
		r.History = []store.Start{}
	}
	for _, id := range nodeOrder(st, wf) {
		ns := st.Nodes[id]
		n := Node{
			ID: id, Label: id, State: ns.State, Attempt: ns.Attempt, ExitCode: ns.ExitCode,
			Reason: ns.Reason, Error: ns.Error, Started: ns.Started, Finished: ns.Finished,
			DurationMS: duration(ns, st.Updated, inUse).Milliseconds(), Log: "", LogTail: nil,
		}
		if wf != nil && wf.Node(id) != nil {
			n.Label = wf.Node(id).DisplayName()
		}
		if ns.Log != "" {
			n.Log = filepath.Join(run.Dir, ns.Log)
		}
		r.Counts[ns.State]++
		r.Nodes = append(r.Nodes, n)
	}
	r.Status = status(r.Counts, inUse)
	return r
}

func nodeOrder(st *store.State, wf *workflow.Workflow) []string {
	var ids []string
	seen := map[string]bool{}
	if wf != nil {
		for _, n := range wf.Nodes {
			if _, ok := st.Nodes[n.ID]; ok {
				ids = append(ids, n.ID)
				seen[n.ID] = true
			}
		}
	}
	var rest []string
	for id := range st.Nodes {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	return append(ids, rest...)
}

// duration is how long a node ran. A node still running in a live run has
// run until now; one that a dead weft left running is measured to the
// last state write, since nobody knows when it stopped.
func duration(ns store.NodeState, updated time.Time, inUse bool) time.Duration {
	switch {
	case ns.Started.IsZero():
		return 0
	case !ns.Finished.IsZero():
		return ns.Finished.Sub(ns.Started)
	case inUse:
		return time.Since(ns.Started)
	default:
		return max(0, updated.Sub(ns.Started))
	}
}

func status(counts map[events.State]int, inUse bool) Status {
	if inUse {
		return Running
	}
	for _, s := range stateOrder {
		if counts[s] > 0 && !s.Terminal() {
			return Interrupted
		}
	}
	switch {
	case counts[events.Failed] > 0:
		return Failed
	case counts[events.Canceled] > 0:
		return Canceled
	case counts[events.Blocked] > 0:
		return Failed
	default:
		return Succeeded
	}
}

// AddLogTails fills LogTail for every node that ran and did not succeed,
// with up to n of its last lines.
func (r *Run) AddLogTails(n int) {
	if n <= 0 {
		return
	}
	for i := range r.Nodes {
		node := &r.Nodes[i]
		if node.Log == "" || node.State == events.Succeeded || node.State == events.Skipped {
			continue
		}
		if lines, err := Tail(node.Log, n); err == nil {
			node.LogTail = lines
		}
	}
}

// tailBytes bounds how much of a log Tail reads, so a node that printed
// gigabytes does not have to be read whole for its last lines.
const tailBytes = 256 * 1024

// Tail returns up to n of the last lines of the file at path, without
// their newlines. A first line cut short by the read limit is dropped.
func Tail(path string, n int) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is a node log inside a run directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(0, info.Size()-tailBytes)
	data := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 {
		return []string{}, nil
	}
	lines := strings.Split(string(data), "\n")
	return lines[max(0, len(lines)-n):], nil
}

// Progress renders the counts as "4 succeeded · 1 failed · 2 blocked".
func (r *Run) Progress() string {
	var parts []string
	for _, s := range stateOrder {
		if c := r.Counts[s]; c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, s))
		}
	}
	return strings.Join(parts, " · ")
}

// Unfinished returns the nodes that did not succeed, skip, or get left
// out, in node order.
func (r *Run) Unfinished() []Node {
	var out []Node
	for _, n := range r.Nodes {
		switch n.State {
		case events.Succeeded, events.Skipped, events.Excluded:
		case events.Pending, events.Ready, events.Running, events.Waiting,
			events.Failed, events.Blocked, events.Canceled:
			out = append(out, n)
		}
	}
	return out
}

// Text renders the run for a terminal or an agent's context: a heading,
// the progress line, one line per node, then the log tail of each node
// that did not succeed and the commands to run next. workflowArg is how
// the user named the workflow, so the suggested commands work as typed.
// latest says whether this is the workflow's newest run, the only one
// --resume continues.
func (r *Run) Text(workflowArg string, latest bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s run %s: %s\n", r.Workflow, r.RunID, r.Status)
	if r.PID > 0 {
		fmt.Fprintf(&b, "in use by weft pid %d\n", r.PID)
	}
	fmt.Fprintf(&b, "started %s, updated %s\n", r.Created.Local().Format(time.DateTime), r.Updated.Local().Format(time.DateTime))
	fmt.Fprintf(&b, "run dir %s\n", r.RunDir)
	if p := r.Progress(); p != "" {
		fmt.Fprintf(&b, "progress: %s\n", p)
	}

	b.WriteString("\n")
	idW, stateW := 0, 0
	for _, n := range r.Nodes {
		idW, stateW = max(idW, len(n.ID)), max(stateW, len(n.State))
	}
	for _, n := range r.Nodes {
		line := fmt.Sprintf("%-*s  %-*s", idW, n.ID, stateW, n.State)
		if n.Attempt > 1 {
			line += fmt.Sprintf("  attempt %d", n.Attempt)
		}
		if !n.Started.IsZero() {
			line += "  " + ui.FormatDuration(time.Duration(n.DurationMS)*time.Millisecond)
		}
		if n.Error != "" {
			line += "  " + n.Error
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}

	for _, n := range r.Nodes {
		if n.LogTail == nil {
			continue
		}
		fmt.Fprintf(&b, "\n%s, attempt %d: %s\nlog %s\n", n.ID, n.Attempt, n.State, n.Log)
		if len(n.LogTail) == 0 {
			b.WriteString("  (the log is empty)\n")
		}
		for _, l := range n.LogTail {
			b.WriteString("  " + l + "\n")
		}
	}

	if next := r.next(workflowArg, latest); len(next) > 0 {
		b.WriteString("\nnext:\n")
		for _, l := range next {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

// next suggests commands for what the run's status leaves to do.
func (r *Run) next(workflowArg string, latest bool) []string {
	var out []string
	unfinished := r.Unfinished()
	for _, n := range unfinished {
		if n.Log != "" && n.State != events.Running {
			out = append(out, fmt.Sprintf("weft logs %s %s --run %s    # full output of %s", workflowArg, n.ID, r.RunID, n.ID))
			break
		}
	}
	if !latest {
		return out
	}
	switch r.Status {
	case Failed, Canceled, Interrupted:
		out = append(out, fmt.Sprintf("weft run %s --resume    # rerun what did not succeed; skip the rest", workflowArg))
	case Running:
		out = append(out, fmt.Sprintf("weft status %s    # again later; the run is still going", workflowArg))
	case Succeeded:
	}
	return out
}

// maxLineNodes caps the unfinished nodes a `weft runs` row names.
const maxLineNodes = 3

// Line renders the run as one row of `weft runs`.
func (r *Run) Line() string {
	mode := ""
	if len(r.History) > 0 {
		mode = r.History[len(r.History)-1].Mode
	}
	var stuck []string
	unfinished := r.Unfinished()
	for _, n := range unfinished[:min(len(unfinished), maxLineNodes)] {
		stuck = append(stuck, n.ID+" "+string(n.State))
	}
	if more := len(unfinished) - maxLineNodes; more > 0 {
		stuck = append(stuck, fmt.Sprintf("%d more", more))
	}
	line := fmt.Sprintf("%s  %-11s  %s  %-6s  %s", r.RunID, r.Status, r.Created.Local().Format(time.DateTime), mode, r.Progress())
	if len(stuck) > 0 {
		line += "  (" + strings.Join(stuck, ", ") + ")"
	}
	return line
}
