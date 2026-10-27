// Package events defines the engine's event stream and a fan-out bus that
// delivers it to UIs.
package events

import (
	"time"
)

// State is a node's lifecycle state.
type State string

// Node states. A node starts pending and turns ready once its needs
// succeed. A ready interactive node waits while other nodes run, then runs
// with the terminal. A running node ends succeeded, failed, or canceled.
// A node that has not started can instead end blocked (a need failed) or
// canceled. Skipped and excluded nodes start in that state and never run.
// A rerun puts a finished node back to pending with the next attempt.
const (
	Pending   State = "pending"
	Ready     State = "ready"
	Running   State = "running"
	Waiting   State = "waiting"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Blocked   State = "blocked"
	Canceled  State = "canceled"
	Skipped   State = "skipped"
	// Excluded nodes are outside the selection of --from or --only, or
	// left out of a resume, and do not run in this attempt.
	Excluded State = "excluded"
)

// Terminal reports whether the state is final for the current attempt.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Blocked, Canceled, Skipped, Excluded:
		return true
	default:
		return false
	}
}

// Reason says why a node reached its terminal state.
type Reason string

// Node end reasons.
const (
	// ReasonExit: the process exited on its own, with any exit code.
	ReasonExit Reason = "exit"
	// ReasonTimeout: the node ran past its timeout and was stopped.
	ReasonTimeout Reason = "timeout"
	// ReasonCanceled: a cancel stopped the node, or it never started.
	ReasonCanceled Reason = "canceled"
	// ReasonBlocked: a need did not succeed, so the node never started.
	ReasonBlocked Reason = "blocked"
	// ReasonStartError: weft could not start the process.
	ReasonStartError Reason = "start_error"
	// ReasonSkipped: a resume kept the node's result from an earlier
	// attempt: a success, or a failure its continue_on_error allowed.
	ReasonSkipped Reason = "skipped"
	// ReasonExcluded: the node is outside the selection of this start.
	ReasonExcluded Reason = "excluded"
	// ReasonAbandoned: the weft that ran the attempt died before it ended,
	// and a later start found the attempt gone.
	ReasonAbandoned Reason = "abandoned"
)

// Event is one of RunStarted, NodeChanged, NodeOutput, OutputDropped,
// RunFinished, or Barrier.
//
//sumtype:decl
type Event interface {
	isEvent()
}

// RunStarted begins each pass of a run. The first pass is followed by a
// NodeChanged for every node; a later pass starts when a node is rerun
// after the previous pass finished.
type RunStarted struct {
	Workflow string `json:"workflow"`
	RunID    string `json:"run_id"`
	RunDir   string `json:"run_dir"`
	// Pass counts the passes of this weft process, from 1.
	Pass int       `json:"pass"`
	At   time.Time `json:"at"`
}

// NodeChanged reports a node state transition.
type NodeChanged struct {
	Node    string `json:"node"`
	State   State  `json:"state"`
	Attempt int    `json:"attempt"`
	// ExitCode is set for finished nodes; -1 when the process did not exit
	// normally.
	ExitCode int `json:"exit_code"`
	// Err explains failures that are not a plain non-zero exit, such as a
	// timeout or a start error.
	Err string `json:"error,omitempty"`
	// Reason is set for terminal states.
	Reason Reason `json:"reason,omitempty"`
	// Started is set once the node starts running.
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	At       time.Time `json:"at"`
}

// NodeOutput carries one line of a node's combined stdout and stderr,
// without the trailing newline. Bare carriage returns are preserved.
type NodeOutput struct {
	Node string `json:"node"`
	Line string `json:"line"`
}

// RunFinished ends each pass of a run.
type RunFinished struct {
	// Success is false when any node failed, was blocked, or was canceled.
	Success bool      `json:"success"`
	At      time.Time `json:"at"`
}

// OutputDropped reports output lines a subscriber never saw because it fell
// behind (for example, stdout is a paused pipe). The per-attempt log files
// are written directly by the engine and still hold every line.
type OutputDropped struct {
	Node  string `json:"node"`
	Lines int    `json:"lines"`
}

// Barrier is delivered to one subscriber after every event published before
// it. The subscriber calls Done once it has handled those events. It never
// reaches other subscribers and carries no run data.
type Barrier struct {
	done chan struct{}
}

// Done marks the barrier reached. It is safe to call more than once.
func (b Barrier) Done() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

func (RunStarted) isEvent()    {}
func (NodeChanged) isEvent()   {}
func (NodeOutput) isEvent()    {}
func (RunFinished) isEvent()   {}
func (OutputDropped) isEvent() {}
func (Barrier) isEvent()       {}
