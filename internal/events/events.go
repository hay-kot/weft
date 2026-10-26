// Package events defines the engine's event stream and a fan-out bus that
// delivers it to UIs.
package events

import (
	"sync"
	"time"
)

// State is a node's lifecycle state.
type State string

// Node states. See the design doc for the transition rules.
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
)

// Terminal reports whether the state is final for the current attempt.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Blocked, Canceled, Skipped:
		return true
	default:
		return false
	}
}

// Event is one of RunStarted, NodeChanged, NodeOutput, or RunFinished.
//
//sumtype:decl
type Event interface {
	isEvent()
}

// RunStarted is the first event of a run.
type RunStarted struct {
	Workflow string    `json:"workflow"`
	RunID    string    `json:"run_id"`
	RunDir   string    `json:"run_dir"`
	At       time.Time `json:"at"`
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
	// Reason is set for terminal states: exit, timeout, canceled, blocked,
	// start_error, or skipped.
	Reason string `json:"reason,omitempty"`
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

// RunFinished is the last event of a run.
type RunFinished struct {
	// Success is false when any node failed, was blocked, or was canceled.
	Success bool      `json:"success"`
	At      time.Time `json:"at"`
}

func (RunStarted) isEvent()  {}
func (NodeChanged) isEvent() {}
func (NodeOutput) isEvent()  {}
func (RunFinished) isEvent() {}

// Bus fans events out to every subscriber. Publish blocks until each
// subscriber's buffer has room, so a slow subscriber applies back-pressure
// instead of losing events.
type Bus struct {
	mu     sync.Mutex
	subs   []chan Event
	closed bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{mu: sync.Mutex{}, subs: nil, closed: false}
}

// Subscribe returns a channel that receives every event published after the
// call. The channel closes when the bus closes.
func (b *Bus) Subscribe(buffer int) <-chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Event, buffer)
	if b.closed {
		close(ch)
		return ch
	}
	b.subs = append(b.subs, ch)
	return ch
}

// Publish delivers e to every subscriber. Publishing after Close is a no-op.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for _, ch := range b.subs {
		ch <- e
	}
}

// Close closes every subscriber channel.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
}
