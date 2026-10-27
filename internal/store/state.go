package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/hay-kot/weft/internal/events"
)

// StateFile is the name of a run's state file.
const StateFile = "state.json"

// StateVersion is the state.json format this weft reads and writes.
const StateVersion = 1

// Start modes recorded in State.History.
const (
	ModeRun    = "run"
	ModeResume = "resume"
	ModeFrom   = "from"
	ModeOnly   = "only"
	ModeRerun  = "rerun"
)

// ErrNoState means a run directory has no state.json.
var ErrNoState = errors.New("no state.json")

// State is a run's persisted state: enough to resume it, and to tell
// afterwards what each weft process did with it.
type State struct {
	Version  int    `json:"version"`
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow"`
	// WorkflowPath is the absolute path of the workflow file.
	WorkflowPath string    `json:"workflow_path"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	// History lists every request that started work in this run, oldest
	// first: the first run, each resume, --from, or --only, and each rerun
	// from the TUI.
	History []Start              `json:"history"`
	Nodes   map[string]NodeState `json:"nodes"`
}

// Start is one entry of State.History.
type Start struct {
	At  time.Time `json:"at"`
	PID int       `json:"pid"`
	// Mode is run, resume, from, only, or rerun.
	Mode string `json:"mode"`
	// Nodes is the selection for from, only, and rerun.
	Nodes []string `json:"nodes,omitempty"`
	// WorkflowHash is the hash of the workflow file weft ran with.
	WorkflowHash string `json:"workflow_hash"`
	// GitHead is the commit checked out at the workflow root, or empty
	// outside a git repository.
	GitHead string `json:"git_head,omitempty"`
	// Forced is set when the user went ahead although the workflow file or
	// git HEAD had changed since the previous start.
	Forced bool `json:"forced,omitempty"`
}

// NodeState is one node's latest state.
type NodeState struct {
	State events.State `json:"state"`
	// Attempt is the node's latest attempt. For a pending node it is the
	// attempt it will run as; zero means the node never ran in this run.
	Attempt  int           `json:"attempt"`
	ExitCode int           `json:"exit_code"`
	Reason   events.Reason `json:"reason,omitempty"`
	Error    string        `json:"error,omitempty"`
	Started  time.Time     `json:"started,omitzero"`
	Finished time.Time     `json:"finished,omitzero"`
	// Log is the attempt's output log, relative to the run directory.
	Log string `json:"log,omitempty"`
	// PGID is the process group of a running attempt. A resume checks it
	// to tell whether an attempt that weft lost track of is still alive.
	PGID int `json:"pgid,omitempty"`
}

// NewState returns the state of a run that has not started any node.
func NewState(run *Run, workflowPath string, now time.Time) *State {
	return &State{
		Version:      StateVersion,
		RunID:        run.ID,
		Workflow:     run.Workflow,
		WorkflowPath: workflowPath,
		Created:      now,
		Updated:      now,
		History:      nil,
		Nodes:        map[string]NodeState{},
	}
}

// Last returns the newest History entry, or a zero Start when there is
// none.
func (s *State) Last() Start {
	if len(s.History) == 0 {
		return Start{At: time.Time{}, PID: 0, Mode: "", Nodes: nil, WorkflowHash: "", GitHead: "", Forced: false}
	}
	return s.History[len(s.History)-1]
}

// Clone returns a deep copy, so a writer can keep mutating its own copy
// while another goroutine reads this one.
func (s *State) Clone() *State {
	c := *s
	c.History = slices.Clone(s.History)
	for i := range c.History {
		c.History[i].Nodes = slices.Clone(c.History[i].Nodes)
	}
	c.Nodes = maps.Clone(s.Nodes)
	return &c
}

// StatePath returns the path of the run's state.json.
func (r *Run) StatePath() string { return filepath.Join(r.Dir, StateFile) }

// WriteState replaces state.json atomically and durably: a crash or power
// loss leaves either the old file or the new one, never a torn mix. Resume
// decides from this file which nodes to skip, so a lost update could rerun
// a release.
func (r *Run) WriteState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(r.StatePath(), append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", r.StatePath(), err)
	}
	return nil
}

// maxStateBytes bounds a state.json read. A real one is a few KB per node.
const maxStateBytes = 64 << 20

// LoadState reads state.json. It returns an error wrapping ErrNoState when
// the file does not exist.
func (r *Run) LoadState() (*State, error) {
	path := r.StatePath()
	data, err := readRegular(path, maxStateBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("run %s: %w", r.ID, ErrNoState)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s is not valid state: %w", path, err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("%s has state version %d; this weft reads version %d", path, s.Version, StateVersion)
	}
	if s.Nodes == nil {
		s.Nodes = map[string]NodeState{}
	}
	return &s, nil
}
