package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		Started: at, Finished: at.Add(time.Second), Log: "nodes/release/2.log", PGID: 0,
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
