// Package store owns a run's directory on disk: weft's own log, the event
// log, and per-node, per-attempt output logs and records.
//
// Layout:
//
//	.weft/runs/<workflow>/<run-id>/
//	  weft.log                 weft's JSON log for this run
//	  state.json               node states and history, for resume
//	  lock                     held by the weft process using the run
//	  events.jsonl            engine events, one JSON object per line
//	  nodes/<node>/<n>.log     combined output of attempt n
//	  nodes/<node>/<n>.json    AttemptRecord for attempt n
package store

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hay-kot/weft/internal/events"
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
	idLayout = "20060102T150405.000"
)

// Run is one run directory.
type Run struct {
	ID string
	// Dir is the absolute path of the run directory.
	Dir      string
	Workflow string
}

// NewID returns a run ID that sorts by start time: a UTC timestamp to the
// millisecond plus a random suffix that keeps IDs from the same millisecond
// apart. Two IDs from the same millisecond sort by suffix, not by start;
// Create avoids that.
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

// Create makes a new run directory for the workflow under root.
func Create(root, workflowName string, now time.Time) (*Run, error) {
	runsDir := WorkflowRunsDir(root, workflowName)
	ids, err := runIDs(runsDir)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	if err := os.MkdirAll(runsDir, dirMode); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	for tries := 0; ; tries++ {
		id, err := NewID(after(ids, now))
		if err != nil {
			return nil, fmt.Errorf("generate run ID: %w", err)
		}
		dir := filepath.Join(runsDir, id)
		err = os.Mkdir(dir, dirMode)
		if errors.Is(err, fs.ErrExist) && tries < idCollisionRetries {
			// Another weft drew the same ID in the same millisecond.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create run directory: %w", err)
		}
		if err := os.Mkdir(filepath.Join(dir, NodesDir), dirMode); err != nil {
			return nil, fmt.Errorf("create run directory: %w", err)
		}
		return &Run{ID: id, Dir: dir, Workflow: workflowName}, nil
	}
}

// idCollisionRetries is how often Create draws a new ID when a directory
// with the drawn one exists. With two random bytes per ID, one retry is
// almost always enough.
const idCollisionRetries = 3

// after returns now, or a millisecond past the newest run in ids when now
// is not later than it. Latest and Prune go by ID order, so a run started
// in the same millisecond as the last one, or after the clock stepped
// back, must still sort last.
func after(ids []string, now time.Time) time.Time {
	if len(ids) == 0 {
		return now
	}
	newest, err := idTime(ids[len(ids)-1])
	if err != nil || now.Truncate(time.Millisecond).After(newest) {
		return now
	}
	return newest.Add(time.Millisecond)
}

// EnsureGitignore keeps `runs/` in `.weft/.gitignore`. An existing file
// that lacks it, as a repository may ship, gets the line appended: node logs
// hold command output, and a `git add -A` must not commit them. A run does
// not depend on this, so a caller treats an error as a warning.
func EnsureGitignore(root string) error {
	dir := filepath.Join(root, workflow.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // .weft holds workflow files the user edits
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path) //nolint:gosec // the user's own .gitignore
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if ignoresRuns(data) {
		return nil
	}
	line := RunsDir + "/\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // a .gitignore is meant to be readable
	if err != nil {
		return fmt.Errorf("add %s to %s: %w", RunsDir+"/", path, err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}

// ignoresRuns reports whether a .gitignore has a pattern for the runs
// directory itself.
func ignoresRuns(data []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case RunsDir, RunsDir + "/", "/" + RunsDir, "/" + RunsDir + "/":
			return true
		}
	}
	return false
}

// Prune deletes every run directory of a workflow except the newest keep.
// Directory names that are not run IDs are left alone, and so is an older
// run another weft process holds locked, so more than keep can remain
// until that process lets go. Newer runs never go in place of a locked one:
// the newest run is the one --resume continues.
func Prune(root, workflowName string, keep int) error {
	dir := WorkflowRunsDir(root, workflowName)
	ids, err := runIDs(dir)
	if err != nil {
		return err
	}
	for i := 0; len(ids)-i > keep; i++ {
		run := &Run{ID: ids[i], Dir: filepath.Join(dir, ids[i]), Workflow: workflowName}
		lock, err := run.Lock()
		if err != nil {
			continue
		}
		err = os.RemoveAll(run.Dir)
		lock.Release()
		if err != nil {
			return fmt.Errorf("prune run %s: %w", ids[i], err)
		}
	}
	return nil
}

func isRunID(name string) bool {
	if len(name) != len(idLayout)+5 || name[len(idLayout)] != '-' {
		return false
	}
	if _, err := hex.DecodeString(name[len(idLayout)+1:]); err != nil {
		return false
	}
	_, err := idTime(name)
	return err == nil
}

func idTime(id string) (time.Time, error) {
	return time.Parse(idLayout, id[:min(len(id), len(idLayout))])
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
	return appendRegular(r.LogPath())
}

// OpenRegular opens a file in a run directory for reading. It refuses
// anything but a regular file: a run directory can come from a checkout,
// and a symlink there to /dev/zero or a terminal would hang a read.
func OpenRegular(path string) (*os.File, error) {
	if err := mustBeRegular(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // callers pass paths inside a run directory
}

// appendRegular opens a regular file for appending, creating it if needed,
// and puts it at fileMode even when it existed with a wider mode.
func appendRegular(path string) (*os.File, error) {
	if err := mustBeRegular(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, fileMode) //nolint:gosec // callers pass paths inside a run directory
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// mustBeRegular fails for a path that exists and is not a regular file. The
// path itself is not followed, so a symlink fails here.
func mustBeRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

// readRegular reads a regular file of at most limit bytes.
func readRegular(path string, limit int64) ([]byte, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
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
	// Reason is why the attempt ended.
	Reason   events.Reason `json:"reason"`
	ExitCode int           `json:"exit_code"`
	Signal   string        `json:"signal,omitempty"`
	Error    string        `json:"error,omitempty"`
	Started  time.Time     `json:"started,omitzero"`
	Finished time.Time     `json:"finished"`
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

// writeFileAtomic replaces path with data through a synced temporary file
// and a rename, then syncs the directory so the rename itself survives a
// crash.
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is a run directory weft created
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
