package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Lock files are small; readPID reads at most this much.
const maxPIDBytes = 64

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

// lockRetryFor bounds how long Lock waits out a status reader's probe. InUse
// holds a shared lock for an instant; without the wait, `weft status` run at
// the wrong moment would make a `--resume` fail as if the run were in use.
const (
	lockRetryFor  = 200 * time.Millisecond
	lockRetryStep = 10 * time.Millisecond
)

// Lock takes the run's lock. It returns a *LockedError when another process
// holds it for longer than a probe takes.
func (r *Run) Lock() (*Lock, error) {
	path := filepath.Join(r.Dir, LockFile)
	if err := mustBeRegular(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open run lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, fileMode) //nolint:gosec // path is inside a run directory
	if err != nil {
		return nil, fmt.Errorf("open run lock: %w", err)
	}
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open run lock: %w", err)
	}
	deadline := time.Now().Add(lockRetryFor)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock run %s: %w", r.ID, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &LockedError{RunID: r.ID, PID: readPID(path)}
		}
		time.Sleep(lockRetryStep)
	}
	// The PID is only for the message another weft shows; the flock is
	// what excludes.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// InUse reports whether another weft process holds the run's lock, and
// that process's ID when known. It probes with a shared lock for an
// instant, so a weft that tries to take the run at that same instant is
// refused as if the run were in use; status readers accept that.
func (r *Run) InUse() (pid int, inUse bool) {
	path := filepath.Join(r.Dir, LockFile)
	f, err := OpenRegular(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return readPID(path), errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0, false
}

// Release gives the lock up. It is safe to call on a nil Lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// The PID goes with the lock, so a stale one is never shown as the
	// holder of a run nobody has.
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

func readPID(path string) int {
	data, err := readRegular(path, maxPIDBytes)
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

// List returns the runs of a workflow, newest first. It returns an empty
// list, not an error, when the workflow has never run.
func List(root, workflowName string) ([]*Run, error) {
	dir := WorkflowRunsDir(root, workflowName)
	ids, err := runIDs(dir)
	if err != nil {
		return nil, err
	}
	runs := make([]*Run, 0, len(ids))
	for _, id := range slices.Backward(ids) {
		runs = append(runs, &Run{ID: id, Dir: filepath.Join(dir, id), Workflow: workflowName})
	}
	return runs, nil
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
