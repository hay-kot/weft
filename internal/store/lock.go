package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// LockFile is the name of the lock file in a run directory.
const LockFile = "lock"

// ErrNoRuns means a workflow has no run directories yet.
var ErrNoRuns = errors.New("no earlier runs")

// LockedError means another weft process holds a run's lock.
type LockedError struct {
	RunID string
	// PID is the holder's process ID, or zero when it is unknown.
	PID int
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("run %s is in use by another weft process (pid %d)", e.RunID, e.PID)
	}
	return fmt.Sprintf("run %s is in use by another weft process", e.RunID)
}

// Lock is an exclusive hold on a run directory. The kernel releases it
// when the process exits, however it exits, so a weft that was killed never
// leaves a run locked.
type Lock struct {
	f *os.File
}

// Lock takes the run's lock without waiting. It returns a *LockedError
// when another process holds it.
func (r *Run) Lock() (*Lock, error) {
	path := filepath.Join(r.Dir, LockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode) //nolint:gosec // path is inside a run directory
	if err != nil {
		return nil, fmt.Errorf("open run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &LockedError{RunID: r.ID, PID: readPID(path)}
		}
		return nil, fmt.Errorf("lock run %s: %w", r.ID, err)
	}
	// The PID is only for the message another weft shows; the flock is
	// what excludes.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release gives the lock up. It is safe to call on a nil Lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

func readPID(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // path is inside a run directory
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// Open returns the existing run of a workflow with the given ID.
func Open(root, workflowName, id string) (*Run, error) {
	if !isRunID(id) {
		return nil, fmt.Errorf("%q is not a run ID", id)
	}
	dir := filepath.Join(WorkflowRunsDir(root, workflowName), id)
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("run %s of %s does not exist", id, workflowName)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return &Run{ID: id, Dir: dir, Workflow: workflowName}, nil
}

// Latest returns the newest run of a workflow. It returns an error wrapping
// ErrNoRuns when there is none.
func Latest(root, workflowName string) (*Run, error) {
	ids, err := runIDs(WorkflowRunsDir(root, workflowName))
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: %w", workflowName, ErrNoRuns)
	}
	return Open(root, workflowName, ids[len(ids)-1])
}

// runIDs returns the run IDs under dir, oldest first.
func runIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isRunID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}
