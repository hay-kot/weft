package plan

import (
	"fmt"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/workflow"
)

// RerunError explains why a node can't be rerun right now.
type RerunError struct {
	Node   string
	Reason string
}

func (e *RerunError) Error() string {
	return fmt.Sprintf("can't rerun %s: %s", e.Node, e.Reason)
}

// active reports whether a node in state s is queued or running in the
// current attempt.
func active(s events.State) bool {
	return !s.Terminal()
}

// Rerun returns the nodes to reset when the user reruns id, alone or with
// everything downstream of it. states holds every node's current state.
//
// id must have finished, and each of its needs must be satisfied, since a
// rerun never reaches upstream. With downstream, no node downstream of id
// may be running or waiting for the terminal: weft can't restart a node
// that has not stopped. Downstream nodes that are still pending are left
// alone; they wait for id again. The result lists id first, then the
// finished downstream nodes in declaration order.
func Rerun(wf *workflow.Workflow, states map[string]events.State, id string, downstream bool) ([]string, error) {
	n := wf.Node(id)
	if n == nil {
		return nil, &RerunError{Node: id, Reason: "no such node"}
	}
	if s := states[id]; active(s) {
		return nil, &RerunError{Node: id, Reason: "it is " + string(s)}
	}
	for _, need := range n.Needs {
		if s := states[need]; !Satisfies(wf.Node(need), s) {
			return nil, &RerunError{Node: id, Reason: fmt.Sprintf("it needs %s, which is %s", need, s)}
		}
	}
	out := []string{id}
	if !downstream {
		return out, nil
	}
	for _, d := range wf.Downstream(id) {
		if d == id {
			continue
		}
		switch s := states[d]; s {
		case events.Running, events.Waiting:
			return nil, &RerunError{Node: id, Reason: fmt.Sprintf("%s, downstream of it, is %s", d, s)}
		case events.Pending, events.Ready:
		default:
			out = append(out, d)
		}
	}
	return out, nil
}
