// Package plan decides where each node of a run starts: fresh, resumed
// from an earlier run, or narrowed with --from or --only. It also decides
// whether a node may be rerun from the TUI. Everything here is a pure
// function of the workflow and node states.
package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// Node is where one node starts.
type Node struct {
	// State is Pending, Skipped, or Excluded.
	State events.State
	// Attempt is the attempt a pending node runs as. For a skipped or
	// excluded node it is the latest attempt of the earlier run, or zero
	// when there was none.
	Attempt int
	// Prior is the node's state in the earlier run. A skipped node keeps
	// its times, exit code, and log path from it.
	Prior store.NodeState
}

// Plan is the starting point of every node in a run.
type Plan struct {
	// Mode is one of the store.Mode* values.
	Mode string
	// Selection is the nodes named by --from or --only.
	Selection []string
	// Continue is set when the plan takes results from the earlier run, so
	// the run carries on in that run's directory instead of a new one.
	Continue bool
	Nodes    map[string]Node
}

// Pending returns the nodes that will run, in declaration order.
func (p Plan) Pending(wf *workflow.Workflow) []string {
	var out []string
	for _, n := range wf.Nodes {
		if p.Nodes[n.ID].State == events.Pending {
			out = append(out, n.ID)
		}
	}
	return out
}

// Satisfies reports whether a need of node n in state s lets its
// dependents start.
func Satisfies(n *workflow.Node, s events.State) bool {
	switch s {
	case events.Succeeded, events.Skipped:
		return true
	case events.Failed:
		return n.ContinueOnError
	default:
		return false
	}
}

// Blocks reports whether a need of node n in state s can never be
// satisfied in this attempt.
func Blocks(n *workflow.Node, s events.State) bool {
	switch s {
	case events.Failed:
		return !n.ContinueOnError
	case events.Canceled, events.Blocked, events.Excluded:
		return true
	default:
		return false
	}
}

// succeeded reports whether a node's earlier state lets a resume skip it.
func succeeded(s events.State) bool {
	return s == events.Succeeded || s == events.Skipped
}

// PriorFailed reports whether an earlier state is a failure, counting a
// skipped node that kept a failed attempt as one. A skip carries a soft
// failure forward from one continuation to the next; the exit code shows
// it, and so does the note the engine leaves in Error, which a skip of a
// success never has (a timed-out node can exit 0 on its SIGINT).
func PriorFailed(ps store.NodeState) bool {
	return ps.State == events.Failed || (ps.State == events.Skipped && (ps.ExitCode != 0 || ps.Error != ""))
}

// outcome is a node's earlier state as the engine judged it, with a kept
// soft failure read back as the failure it was.
func outcome(ps store.NodeState) events.State {
	if PriorFailed(ps) {
		return events.Failed
	}
	return ps.State
}

func emptyNodeState() store.NodeState {
	return store.NodeState{
		State: "", Attempt: 0, ExitCode: 0, Reason: "", Error: "",
		Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0,
	}
}

// nextAttempt returns the attempt a node runs as after its earlier state.
// An attempt that never started has no files, so it is reused.
func nextAttempt(prior store.NodeState, ok bool) int {
	if !ok || prior.Attempt == 0 {
		return 1
	}
	switch prior.State {
	case events.Pending, events.Ready, events.Waiting:
		return prior.Attempt
	default:
		return prior.Attempt + 1
	}
}

func pending(prior store.NodeState, ok bool) Node {
	return Node{State: events.Pending, Attempt: nextAttempt(prior, ok), Prior: prior}
}

// Fresh returns the plan of a new run: every node pending at attempt 1.
func Fresh(wf *workflow.Workflow) Plan {
	p := Plan{Mode: store.ModeRun, Selection: nil, Continue: false, Nodes: make(map[string]Node, len(wf.Nodes))}
	for _, n := range wf.Nodes {
		p.Nodes[n.ID] = Node{State: events.Pending, Attempt: 1, Prior: emptyNodeState()}
	}
	return p
}

// Resume returns the plan that carries on the earlier run: nodes that
// succeeded are skipped and the rest run again. A node the earlier state
// shows as running did not finish as far as weft knows (weft was killed),
// so it runs again too. Nodes the workflow gained since then run as new.
// Nodes the last start left out with --from or --only stay out: a resume
// never runs something the user chose not to run, the way it never runs
// against a moved HEAD without being asked.
func Resume(wf *workflow.Workflow, prior *store.State) Plan {
	p := Plan{Mode: store.ModeResume, Selection: nil, Continue: true, Nodes: make(map[string]Node, len(wf.Nodes))}
	for _, n := range wf.Nodes {
		ps, ok := prior.Nodes[n.ID]
		switch {
		case !ok:
			ps = emptyNodeState()
		case succeeded(outcome(ps)):
			p.Nodes[n.ID] = Node{State: events.Skipped, Attempt: ps.Attempt, Prior: ps}
			continue
		case ps.State == events.Excluded:
			p.Nodes[n.ID] = Node{State: events.Excluded, Attempt: ps.Attempt, Prior: ps}
			continue
		}
		p.Nodes[n.ID] = pending(ps, ok)
	}
	return p
}

// From returns the plan that runs id and everything downstream of it.
// prior is the latest run, or nil when there is none.
func From(wf *workflow.Workflow, prior *store.State, id string) (Plan, error) {
	if err := known(wf, []string{id}); err != nil {
		return Plan{}, err
	}
	return selected(wf, prior, store.ModeFrom, []string{id}, wf.Downstream(id))
}

// Only returns the plan that runs exactly ids. prior is the latest run, or
// nil when there is none.
func Only(wf *workflow.Workflow, prior *store.State, ids []string) (Plan, error) {
	if len(ids) == 0 {
		return Plan{}, errors.New("no nodes given")
	}
	if err := known(wf, ids); err != nil {
		return Plan{}, err
	}
	var set []string
	for _, n := range wf.Nodes {
		if slices.Contains(ids, n.ID) {
			set = append(set, n.ID)
		}
	}
	return selected(wf, prior, store.ModeOnly, ids, set)
}

// UnmetNeedError means a selected node needs a node outside the selection
// that did not succeed in the latest run.
type UnmetNeedError struct {
	Node string
	Need string
	// RunID is the latest run, or empty when there is no earlier run.
	RunID string
	// State is the need's state in the latest run, or empty when that run
	// does not know the node.
	State events.State
}

func (e *UnmetNeedError) Error() string {
	switch {
	case e.RunID == "":
		return fmt.Sprintf("%s needs %s, which is not selected, and there is no earlier run to take its result from", e.Node, e.Need)
	case e.State == "":
		return fmt.Sprintf("%s needs %s, which is not selected and never ran in the latest run %s", e.Node, e.Need, e.RunID)
	default:
		return fmt.Sprintf("%s needs %s, which is not selected and did not succeed in the latest run %s (it is %s)", e.Node, e.Need, e.RunID, e.State)
	}
}

// selected plans a run of exactly the nodes in set. Their needs outside
// the set must have let them start in prior: succeeded, or failed with
// continue_on_error, as the engine itself accepts. When set has no such
// needs, nothing is taken from prior and the plan is a new run in which the
// other nodes are excluded. A node downstream of the set is excluded too,
// whatever it did before: its earlier result was against the old run.
func selected(wf *workflow.Workflow, prior *store.State, mode string, selection, set []string) (Plan, error) {
	inSet := make(map[string]bool, len(set))
	for _, id := range set {
		inSet[id] = true
	}
	downstream := map[string]bool{}
	for _, id := range set {
		for _, d := range wf.Downstream(id) {
			if !inSet[d] {
				downstream[d] = true
			}
		}
	}
	var outside []string
	for _, id := range set {
		for _, need := range wf.Node(id).Needs {
			if inSet[need] || slices.Contains(outside, need) {
				continue
			}
			if prior == nil {
				return Plan{}, &UnmetNeedError{Node: id, Need: need, RunID: "", State: ""}
			}
			ps, ok := prior.Nodes[need]
			if !ok || !Satisfies(wf.Node(need), outcome(ps)) {
				return Plan{}, &UnmetNeedError{Node: id, Need: need, RunID: prior.RunID, State: outcome(ps)}
			}
			outside = append(outside, need)
		}
	}

	p := Plan{Mode: mode, Selection: slices.Clone(selection), Continue: len(outside) > 0, Nodes: make(map[string]Node, len(wf.Nodes))}
	for _, n := range wf.Nodes {
		var ps store.NodeState
		ok := false
		if p.Continue {
			ps, ok = prior.Nodes[n.ID]
		}
		if !ok {
			ps = emptyNodeState()
		}
		switch {
		case inSet[n.ID]:
			p.Nodes[n.ID] = pending(ps, ok)
		case ok && !downstream[n.ID] && Satisfies(n, outcome(ps)):
			p.Nodes[n.ID] = Node{State: events.Skipped, Attempt: ps.Attempt, Prior: ps}
		default:
			p.Nodes[n.ID] = Node{State: events.Excluded, Attempt: ps.Attempt, Prior: ps}
		}
	}
	return p, nil
}

func known(wf *workflow.Workflow, ids []string) error {
	for _, id := range ids {
		if wf.Node(id) == nil {
			names := make([]string, 0, len(wf.Nodes))
			for _, n := range wf.Nodes {
				names = append(names, n.ID)
			}
			return fmt.Errorf("workflow %s has no node %q (nodes: %s)", wf.Name, id, strings.Join(names, ", "))
		}
	}
	return nil
}

// Lost returns the nodes the earlier state shows as running: the
// workflow's nodes in declaration order, then nodes the workflow no longer
// has, by ID. weft exited without seeing them finish, and a removed node's
// process may be alive all the same.
func Lost(wf *workflow.Workflow, prior *store.State) []string {
	var out []string
	for _, n := range wf.Nodes {
		if ps, ok := prior.Nodes[n.ID]; ok && ps.State == events.Running {
			out = append(out, n.ID)
		}
	}
	// state.json may come from a checkout, and these IDs become file paths
	// in the attempt records, so only well-formed ones are taken.
	var gone []string
	for id, ps := range prior.Nodes {
		if wf.Node(id) == nil && ps.State == events.Running && workflow.ValidID(id) {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	return append(out, gone...)
}
