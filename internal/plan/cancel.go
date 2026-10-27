package plan

import (
	"fmt"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/workflow"
)

// CancelAction is what canceling one node does.
type CancelAction int

const (
	// CancelUnstarted marks a node that has not started as canceled.
	CancelUnstarted CancelAction = iota + 1
	// CancelRunning starts the node's graceful shutdown: SIGINT, its
	// kill_timeout, SIGTERM, then SIGKILL.
	CancelRunning
	// KillRunning sends SIGKILL to a running node that is already shutting
	// down, skipping what is left of its grace period.
	KillRunning
)

func (a CancelAction) String() string {
	switch a {
	case CancelUnstarted:
		return "cancel_unstarted"
	case CancelRunning:
		return "cancel_running"
	case KillRunning:
		return "kill_running"
	default:
		return "none"
	}
}

// CancelError explains why a node can't be canceled.
type CancelError struct {
	Node   string
	Reason string
}

func (e *CancelError) Error() string {
	return fmt.Sprintf("can't cancel %s: %s", e.Node, e.Reason)
}

// Cancel returns what canceling id does. states holds every node's current
// state; stopping reports whether id's current attempt was already asked to
// stop, by an earlier Cancel or by a cancel of the whole run.
//
// A second cancel of a running node kills it, the same way a second Ctrl-C
// kills the run: a node stuck in its cleanup must not hold the user for its
// whole kill_timeout. A node that finished can't be canceled.
func Cancel(wf *workflow.Workflow, states map[string]events.State, id string, stopping bool) (CancelAction, error) {
	if wf.Node(id) == nil {
		return 0, &CancelError{Node: id, Reason: "no such node"}
	}
	switch s := states[id]; s {
	case events.Pending, events.Ready, events.Waiting:
		return CancelUnstarted, nil
	case events.Running:
		if stopping {
			return KillRunning, nil
		}
		return CancelRunning, nil
	default:
		return 0, &CancelError{Node: id, Reason: "it is " + string(s)}
	}
}
