package tui

import (
	"time"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/ui"
	"github.com/hay-kot/weft/internal/workflow"
)

// nodeStatus is the TUI's copy of one node's latest NodeChanged event.
type nodeStatus struct {
	State    events.State
	Attempt  int
	ExitCode int
	Err      string
	Reason   events.Reason
	Started  time.Time
	Finished time.Time
}

// runState is everything the TUI knows about the run, built only from
// engine events. Components read it; only Model.applyEvent writes it.
type runState struct {
	wf       *workflow.Workflow
	run      *store.Run
	started  time.Time
	finished time.Time
	done     bool
	success  bool
	nodes    map[string]*nodeStatus
	logs     map[string]*logBuffer
}

// newRunState returns the state before any event. With a plan, nodes start
// as planned, so a run waiting for confirmation already shows what it
// will skip.
func newRunState(wf *workflow.Workflow, run *store.Run, p *plan.Plan, logCap int) *runState {
	rs := &runState{
		wf:       wf,
		run:      run,
		started:  time.Time{},
		finished: time.Time{},
		done:     false,
		success:  false,
		nodes:    make(map[string]*nodeStatus, len(wf.Nodes)),
		logs:     make(map[string]*logBuffer, len(wf.Nodes)),
	}
	for _, n := range wf.Nodes {
		st := &nodeStatus{
			State: events.Pending, Attempt: 1, ExitCode: 0, Err: "", Reason: "",
			Started: time.Time{}, Finished: time.Time{},
		}
		if p != nil {
			if pn, ok := p.Nodes[n.ID]; ok {
				st.State, st.Attempt = pn.State, pn.Attempt
				if pn.State != events.Pending {
					st.Started, st.Finished, st.ExitCode = pn.Prior.Started, pn.Prior.Finished, pn.Prior.ExitCode
				}
			}
		}
		rs.nodes[n.ID] = st
		rs.logs[n.ID] = newLogBuffer(logCap)
	}
	return rs
}

// states returns every node's state.
func (rs *runState) states() map[string]events.State {
	out := make(map[string]events.State, len(rs.nodes))
	for id, st := range rs.nodes {
		out[id] = st.State
	}
	return out
}

// state returns a node's state, or pending for an unknown node.
func (rs *runState) state(id string) events.State {
	if st, ok := rs.nodes[id]; ok {
		return st.State
	}
	return events.Pending
}

// elapsed returns how long the run has taken so far.
func (rs *runState) elapsed(now time.Time) time.Duration {
	if rs.started.IsZero() {
		return 0
	}
	end := now
	if rs.done {
		end = rs.finished
	}
	return max(0, end.Sub(rs.started))
}

// finishedCount returns how many nodes are in a terminal state.
func (rs *runState) finishedCount() int {
	n := 0
	for _, st := range rs.nodes {
		if st.State.Terminal() {
			n++
		}
	}
	return n
}

// counts returns how many nodes are in each state.
func (rs *runState) counts() map[events.State]int {
	out := map[events.State]int{}
	for _, st := range rs.nodes {
		out[st.State]++
	}
	return out
}

// nodeDuration returns how long a node has run and whether it has started.
func nodeDuration(st *nodeStatus, now time.Time) (time.Duration, bool) {
	if st == nil || st.Started.IsZero() || st.Reason == events.ReasonStartError {
		return 0, false
	}
	end := now
	if !st.Finished.IsZero() {
		end = st.Finished
	}
	return max(0, end.Sub(st.Started)), true
}

// icon returns the graph icon for a state. Running nodes show a spinner
// frame in its place when rendered.
func icon(s events.State) string {
	switch s {
	case events.Running:
		return "●"
	case events.Waiting:
		return "◆"
	case events.Succeeded, events.Skipped:
		return "✓"
	case events.Failed:
		return "✗"
	case events.Blocked, events.Canceled:
		return "⊘"
	case events.Excluded:
		return "·"
	case events.Pending, events.Ready:
		return "○"
	default:
		return "○"
	}
}

// spinnerFrames animate running nodes. Every frame is one cell wide, the
// same as the "●" the layout reserves.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is how long each spinner frame shows.
const spinnerInterval = 100 * time.Millisecond

// spinnerFrame picks the frame for now, so the spinner is a pure function
// of the clock.
func spinnerFrame(now time.Time) string {
	n := now.UnixMilli() / spinnerInterval.Milliseconds()
	return spinnerFrames[int(n%int64(len(spinnerFrames)))]
}

// cellFor returns the graph cell text for a node: icon, label, and the
// duration once it has started.
func cellFor(n *workflow.Node, st *nodeStatus, now time.Time) layout.Cell {
	state := events.Pending
	if st != nil {
		state = st.State
	}
	suffix := ""
	if d, ok := nodeDuration(st, now); ok {
		suffix = " " + ui.FormatDuration(d)
	}
	return layout.Cell{Prefix: icon(state) + " ", Label: n.DisplayName(), Suffix: suffix}
}

// cells returns the graph cells for every node.
func (rs *runState) cells(now time.Time) map[string]layout.Cell {
	out := make(map[string]layout.Cell, len(rs.wf.Nodes))
	for _, n := range rs.wf.Nodes {
		out[n.ID] = cellFor(n, rs.nodes[n.ID], now)
	}
	return out
}

// sticky reports whether automatic selection should stay on a node in this
// state: it is doing something worth watching, or it failed.
func sticky(s events.State) bool {
	switch s {
	case events.Running, events.Waiting, events.Failed:
		return true
	default:
		return false
	}
}

// autoSelect returns the node automatic selection should show after node
// changed to its current state. It returns selected unchanged when there is
// no better choice. Selection follows work as it starts and stops on a
// failure, so a run left alone ends showing what went wrong.
func (rs *runState) autoSelect(selected, changed string) string {
	cur := rs.state(selected)
	next := rs.state(changed)
	switch next {
	case events.Running, events.Waiting:
		if !sticky(cur) {
			return changed
		}
	case events.Failed:
		if cur != events.Failed {
			return changed
		}
	case events.Succeeded, events.Skipped, events.Canceled, events.Blocked, events.Excluded:
		if changed == selected {
			for _, n := range rs.wf.Nodes {
				if s := rs.state(n.ID); s == events.Running || s == events.Waiting {
					return n.ID
				}
			}
		}
	case events.Pending, events.Ready:
	}
	return selected
}
