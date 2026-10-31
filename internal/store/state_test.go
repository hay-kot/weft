package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
)

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s := store.NewState(run, "/repo/.weft/deploy.yml", at)
	s.History = append(s.History, store.Start{
		At: at, PID: 42, Mode: store.ModeResume, Nodes: nil,
		WorkflowHash: "abc", GitHead: "def", Forced: true,
	})
	s.Nodes["release"] = store.NodeState{
		State: events.Succeeded, Attempt: 2, ExitCode: 0, Reason: "exit", Error: "",
		Started: at, Finished: at.Add(time.Second), Log: "release.2.log", PGID: 0,
	}
	require.NoError(t, run.WriteState(s))

	info, err := os.Stat(run.StatePath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := run.LoadState()
	require.NoError(t, err)
	assert.Equal(t, s, got)
	assert.Equal(t, store.ModeResume, got.Last().Mode)

	entries, err := os.ReadDir(run.Dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".state.json.", "temp file left behind")
	}
}

func TestLoadStateErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		run := mustCreate(t, t.TempDir(), time.Now())
		_, err := run.LoadState()
		require.ErrorIs(t, err, store.ErrNoState)
	})

	t.Run("not JSON", func(t *testing.T) {
		t.Parallel()

		run := mustCreate(t, t.TempDir(), time.Now())
		require.NoError(t, os.WriteFile(run.StatePath(), []byte("{"), 0o600))
		_, err := run.LoadState()
		require.ErrorContains(t, err, run.StatePath()+" is not valid state")
	})

	t.Run("other version", func(t *testing.T) {
		t.Parallel()

		run := mustCreate(t, t.TempDir(), time.Now())
		require.NoError(t, os.WriteFile(run.StatePath(), []byte(`{"version": 9}`), 0o600))
		_, err := run.LoadState()
		require.ErrorContains(t, err, "state version 9")
	})
}

func TestStateCloneIsDeep(t *testing.T) {
	t.Parallel()

	s := store.NewState(&store.Run{ID: "x", Dir: "/x", Workflow: "deploy"}, "/w", time.Now())
	s.History = []store.Start{{At: time.Time{}, PID: 1, Mode: store.ModeOnly, Nodes: []string{"a"}, WorkflowHash: "", GitHead: "", Forced: false}}
	s.Nodes["a"] = store.NodeState{State: events.Pending, Attempt: 1, ExitCode: 0, Reason: "", Error: "", Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0}

	c := s.Clone()
	c.History[0].Nodes[0] = "b"
	c.Nodes["a"] = store.NodeState{State: events.Failed, Attempt: 1, ExitCode: 1, Reason: "", Error: "", Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0}

	assert.Equal(t, "a", s.History[0].Nodes[0])
	assert.Equal(t, events.Pending, s.Nodes["a"].State)
}

func TestLock(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	lock, err := run.Lock()
	require.NoError(t, err)

	// flock belongs to the open file, so a second open in this process
	// conflicts the same way another process would.
	_, err = run.Lock()
	var locked *store.LockedError
	require.ErrorAs(t, err, &locked)
	assert.Equal(t, run.ID, locked.RunID)
	assert.Equal(t, os.Getpid(), locked.PID)
	assert.Contains(t, err.Error(), "in use by another weft process")

	lock.Release()
	lock.Release()
	again, err := run.Lock()
	require.NoError(t, err)
	again.Release()
}

func TestLatestAndOpen(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, err := store.Latest(root, "deploy")
	require.ErrorIs(t, err, store.ErrNoRuns)

	base := filepath.Join(root, ".weft", "runs", "deploy")
	for _, id := range []string{"20260102T000000.000-bbbb", "20260101T000000.000-aaaa"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, id), 0o700))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(base, "zzz-not-a-run"), 0o700))

	latest, err := store.Latest(root, "deploy")
	require.NoError(t, err)
	assert.Equal(t, "20260102T000000.000-bbbb", latest.ID)
	assert.Equal(t, filepath.Join(base, latest.ID), latest.Dir)
	assert.Equal(t, "deploy", latest.Workflow)

	_, err = store.Open(root, "deploy", "20260103T000000.000-cccc")
	require.ErrorContains(t, err, "does not exist")
	_, err = store.Open(root, "deploy", "../etc")
	require.ErrorContains(t, err, "not a run ID")
}

func TestLatestAfterRunsInTheSameMillisecond(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var created []string
	for range 5 {
		created = append(created, mustCreate(t, root, now).ID)
	}
	assert.True(t, slices.IsSorted(created), "ids %v", created)

	latest, err := store.Latest(root, "deploy")
	require.NoError(t, err)
	assert.Equal(t, created[len(created)-1], latest.ID)

	earlier := mustCreate(t, root, now.Add(-time.Hour))
	assert.Greater(t, earlier.ID, created[len(created)-1], "a clock step back must still sort last")
}

func TestPruneSkipsLockedRuns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	base := filepath.Join(root, ".weft", "runs", "deploy")
	ids := []string{"20260101T000000.000-aaaa", "20260102T000000.000-bbbb", "20260103T000000.000-cccc"}
	for _, id := range ids {
		require.NoError(t, os.MkdirAll(filepath.Join(base, id), 0o700))
	}
	oldest, err := store.Open(root, "deploy", ids[0])
	require.NoError(t, err)
	lock, err := oldest.Lock()
	require.NoError(t, err)
	defer lock.Release()

	require.NoError(t, store.Prune(root, "deploy", 1))

	for i, id := range ids {
		_, err := os.Stat(filepath.Join(base, id))
		gone := errors.Is(err, os.ErrNotExist)
		switch i {
		case 0:
			assert.False(t, gone, "locked run %s was pruned", id)
		case 1:
			assert.True(t, gone, "run %s survived", id)
		default:
			assert.False(t, gone, "newest run %s was pruned", id)
		}
	}
}

func TestWriteEventsContinuesSeq(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	for range 2 {
		ch := make(chan events.Event, 2)
		ch <- events.RunFinished{Success: true, At: time.Time{}}
		ch <- events.RunFinished{Success: false, At: time.Time{}}
		close(ch)
		require.NoError(t, run.WriteEvents(ch))
	}

	data, err := os.ReadFile(run.EventsPath())
	require.NoError(t, err)
	assert.Contains(t, string(data), `"seq":4`)
	assert.NotContains(t, string(data), `"seq":5`)
}

func TestListNewestFirst(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runs, err := store.List(root, "deploy")
	require.NoError(t, err)
	assert.Empty(t, runs)

	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	older := mustCreate(t, root, now)
	newer := mustCreate(t, root, now.Add(time.Minute))
	runs, err = store.List(root, "deploy")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, []string{newer.ID, older.ID}, []string{runs[0].ID, runs[1].ID})
	assert.Equal(t, newer.Dir, runs[0].Dir)
}

func TestInUse(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	_, inUse := run.InUse()
	assert.False(t, inUse, "a run that was never locked")

	lock, err := run.Lock()
	require.NoError(t, err)
	pid, inUse := run.InUse()
	assert.True(t, inUse)
	assert.Equal(t, os.Getpid(), pid)

	lock.Release()
	_, inUse = run.InUse()
	assert.False(t, inUse, "a released lock")
	relock, err := run.Lock()
	require.NoError(t, err, "probing must not leave the run locked")
	relock.Release()
}

func TestLockWaitsOutAShortHold(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	// A status reader's probe: a shared lock from another descriptor, held
	// for a moment.
	probe, err := os.OpenFile(filepath.Join(run.Dir, store.LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(probe.Fd()), syscall.LOCK_SH))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
		_ = probe.Close()
	}()

	lock, err := run.Lock()
	require.NoError(t, err, "a probe's hold is waited out, not reported as another weft")
	lock.Release()
}

func TestReleaseClearsThePID(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	lock, err := run.Lock()
	require.NoError(t, err)
	path := filepath.Join(run.Dir, store.LockFile)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(os.Getpid())+"\n", string(data))

	lock.Release()
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Empty(t, data, "a stale PID must not name a holder nobody is")
}

func TestWriteEventsEndsATornLine(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	torn := `{"seq":1,"time":"t","type":"run_started","event":{}}` + "\n" + `{"seq":2,"ty`
	require.NoError(t, os.WriteFile(run.EventsPath(), []byte(torn), 0o600))

	ch := make(chan events.Event, 1)
	ch <- events.RunFinished{Success: true, At: time.Time{}}
	close(ch)
	require.NoError(t, run.WriteEvents(ch))

	data, err := os.ReadFile(run.EventsPath())
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, `{"seq":2,"ty`, lines[1], "the cut-off record stays on its own line")
	assert.Contains(t, lines[2], `"seq":3`, "the cut-off record took a seq")
}

func TestOpenRejectsABadSuffix(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	run := mustCreate(t, root, time.Now())
	for _, id := range []string{run.ID[:len(run.ID)-4] + "zzzz", run.ID[:len(run.ID)-4] + "/../"} {
		_, err := store.Open(root, "deploy", id)
		require.ErrorContains(t, err, "is not a run ID", id)
	}
	_, err := store.Open(root, "deploy", run.ID)
	require.NoError(t, err)
}

func TestRunFilesMustBeRegular(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(elsewhere, []byte("{}"), 0o600))
	require.NoError(t, os.Symlink(elsewhere, run.StatePath()))
	require.NoError(t, os.Symlink(elsewhere, run.LogPath()))
	require.NoError(t, os.Symlink(elsewhere, run.EventsPath()))

	_, err := run.LoadState()
	require.ErrorContains(t, err, "is not a regular file")
	_, err = run.OpenLog()
	require.ErrorContains(t, err, "is not a regular file")
	_, err = store.OpenRegular(run.StatePath())
	require.ErrorContains(t, err, "is not a regular file")
	ch := make(chan events.Event)
	close(ch)
	require.ErrorContains(t, run.WriteEvents(ch), "is not a regular file")
}

func TestOpenLogTightensTheMode(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	require.NoError(t, os.WriteFile(run.LogPath(), []byte("old\n"), 0o644))
	f, err := run.OpenLog()
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	info, err := os.Stat(run.LogPath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
