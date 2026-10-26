// Package store owns a run's directory on disk: weft's own log, the event
// log, and per-node, per-attempt output logs and records.
//
// Layout:
//
//	.weft/runs/<workflow>/<run-id>/
//	  weft.log                 weft's JSON log for this run
//	  events.jsonl             engine events, one JSON object per line
//	  nodes/<node>/<n>.log     combined output of attempt n
//	  nodes/<node>/<n>.json    AttemptRecord for attempt n
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/hay-kot/weft/internal/workflow"
)

// File and directory names inside a run directory.
const (
	LogFile    = "weft.log"
	EventsFile = "events.jsonl"
	NodesDir   = "nodes"
	RunsDir    = "runs"
)

// KeepRuns is how many runs per workflow survive pruning.
const KeepRuns = 20

const (
	dirMode  = 0o700
	fileMode = 0o600
	idLayout = "20060102T150405"
)

// Run is one run directory.
type Run struct {
	ID string
	// Dir is the absolute path of the run directory.
	Dir      string
	Workflow string
}

// NewID returns a run ID that sorts by start time: a UTC timestamp plus a
// random suffix that keeps runs started in the same second apart.
func NewID(now time.Time) (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format(idLayout) + "-" + hex.EncodeToString(b[:]), nil
}

// WorkflowRunsDir returns the directory that holds every run of a workflow.
func WorkflowRunsDir(root, workflowName string) string {
	return filepath.Join(root, workflow.DirName, RunsDir, workflowName)
}

// Create makes a new run directory for the workflow under root. It also
// writes `.weft/.gitignore` with `runs/` when that file does not exist yet.
func Create(root, workflowName string, now time.Time) (*Run, error) {
	if err := ensureGitignore(root); err != nil {
		return nil, err
	}
	id, err := NewID(now)
	if err != nil {
		return nil, fmt.Errorf("generate run ID: %w", err)
	}
	dir := filepath.Join(WorkflowRunsDir(root, workflowName), id)
	if err := os.MkdirAll(filepath.Join(dir, NodesDir), dirMode); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	return &Run{ID: id, Dir: dir, Workflow: workflowName}, nil
}

func ensureGitignore(root string) error {
	dir := filepath.Join(root, workflow.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // .weft holds workflow files the user edits
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, ".gitignore")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a .gitignore is meant to be readable
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(RunsDir + "/\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}

// Prune deletes the oldest run directories of a workflow so that at most
// keep remain. Directory names that are not run IDs are left alone.
func Prune(root, workflowName string, keep int) error {
	dir := WorkflowRunsDir(root, workflowName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isRunID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	for len(ids) > keep {
		if err := os.RemoveAll(filepath.Join(dir, ids[0])); err != nil {
			return fmt.Errorf("prune run %s: %w", ids[0], err)
		}
		ids = ids[1:]
	}
	return nil
}

func isRunID(name string) bool {
	if len(name) != len(idLayout)+5 || name[len(idLayout)] != '-' {
		return false
	}
	_, err := time.Parse(idLayout, name[:len(idLayout)])
	return err == nil
}

// LogPath returns the path of weft's own log for this run.
func (r *Run) LogPath() string { return filepath.Join(r.Dir, LogFile) }

// EventsPath returns the path of the event log.
func (r *Run) EventsPath() string { return filepath.Join(r.Dir, EventsFile) }

// AttemptLogPath returns the output log path for a node attempt.
func (r *Run) AttemptLogPath(node string, attempt int) string {
	return filepath.Join(r.Dir, NodesDir, node, strconv.Itoa(attempt)+".log")
}

// AttemptRecordPath returns the record path for a node attempt.
func (r *Run) AttemptRecordPath(node string, attempt int) string {
	return filepath.Join(r.Dir, NodesDir, node, strconv.Itoa(attempt)+".json")
}

// OpenLog opens weft's own log for appending.
func (r *Run) OpenLog() (*os.File, error) {
	return os.OpenFile(r.LogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
}

// CreateAttemptLog creates the output log for a node attempt.
func (r *Run) CreateAttemptLog(node string, attempt int) (*os.File, error) {
	path := r.AttemptLogPath(node, attempt)
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode) //nolint:gosec // path is built from a validated node ID
}

// AttemptRecord describes one node attempt after it ends.
type AttemptRecord struct {
	Node        string   `json:"node"`
	Attempt     int      `json:"attempt"`
	Command     string   `json:"command"`
	Shell       []string `json:"shell"`
	Dir         string   `json:"dir"`
	Interactive bool     `json:"interactive"`
	State       string   `json:"state"`
	// Reason is why the attempt ended: exit, timeout, canceled, blocked, or
	// start_error.
	Reason   string    `json:"reason"`
	ExitCode int       `json:"exit_code"`
	Signal   string    `json:"signal,omitempty"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished"`
	// Log is the output log path relative to the run directory. Empty when
	// the attempt never started.
	Log string `json:"log,omitempty"`
}

// WriteAttemptRecord writes the record atomically.
func (r *Run) WriteAttemptRecord(rec AttemptRecord) error {
	path := r.AttemptRecordPath(rec.Node, rec.Attempt)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// RelPath returns path relative to the run directory.
func (r *Run) RelPath(path string) string {
	rel, err := filepath.Rel(r.Dir, path)
	if err != nil {
		return path
	}
	return rel
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
