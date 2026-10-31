package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
)

var runIDPattern = regexp.MustCompile(`^\d{8}T\d{6}\.\d{3}-[0-9a-f]{4}$`)

func mustCreate(t *testing.T, root string, now time.Time) *store.Run {
	t.Helper()
	run, err := store.Create(root, "deploy", now)
	require.NoError(t, err)
	return run
}

func TestNewID(t *testing.T) {
	t.Parallel()

	t.Run("format and UTC timestamp", func(t *testing.T) {
		t.Parallel()

		// 05:06:07 at UTC-5 is 10:06:07 UTC.
		now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("EST", -5*3600))
		id, err := store.NewID(now)
		require.NoError(t, err)
		assert.Regexp(t, runIDPattern, id)
		assert.True(t, strings.HasPrefix(id, "20260304T100607.000-"), "id %q", id)
	})

	t.Run("later times sort later", func(t *testing.T) {
		t.Parallel()

		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		times := []time.Time{
			base,
			base.Add(time.Second),
			base.Add(time.Minute),
			base.Add(24 * time.Hour),
		}
		ids := make([]string, 0, len(times))
		for _, at := range times {
			id, err := store.NewID(at)
			require.NoError(t, err)
			ids = append(ids, id)
		}
		assert.True(t, slices.IsSorted(ids), "ids %v", ids)
	})
}

func TestCreate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	run := mustCreate(t, root, now)

	assert.Equal(t, "deploy", run.Workflow)
	assert.Regexp(t, runIDPattern, run.ID)
	assert.Equal(t, filepath.Join(root, ".weft", "runs", "deploy", run.ID), run.Dir)

	info, err := os.Stat(run.Dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestEnsureGitignoreWritesRuns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, store.EnsureGitignore(root))
	gitignore, err := os.ReadFile(filepath.Join(root, ".weft", ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "runs/\n", string(gitignore))
}

func TestEnsureGitignoreReportsAFileItCannotChange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, ".weft", ".gitignore")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("custom\n"), 0o444))
	err := store.EnsureGitignore(root)
	require.ErrorContains(t, err, "add runs/ to")

	run, err := store.Create(root, "deploy", time.Now())
	require.NoError(t, err, "a run does not depend on the .gitignore")
	assert.DirExists(t, run.Dir)
}

func TestEnsureGitignoreKeepsRunsInAnExistingFile(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, before, after string }{
		{"appends when missing", "custom\n", "custom\nruns/\n"},
		{"ends an unterminated last line first", "custom", "custom\nruns/\n"},
		{"leaves runs/ alone", "custom\nruns/\n", "custom\nruns/\n"},
		{"accepts /runs", "/runs\n", "/runs\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := filepath.Join(root, ".weft", ".gitignore")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(tt.before), 0o600))

			require.NoError(t, store.EnsureGitignore(root))

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.after, string(got))
		})
	}
}

func TestRunPaths(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())

	assert.Equal(t, filepath.Join(run.Dir, "weft.log"), run.LogPath())
	assert.Equal(t, filepath.Join(run.Dir, "events.jsonl"), run.EventsPath())
	assert.Equal(t, filepath.Join(run.Dir, "build.2.log"), run.AttemptLogPath("build", 2))
	assert.Equal(t, filepath.Join(run.Dir, "build.2.json"), run.AttemptRecordPath("build", 2))
	assert.Equal(t, "build.2.log", run.RelPath(run.AttemptLogPath("build", 2)))
}

func TestAttemptFilesAreDirectlyInTheRunDirectory(t *testing.T) {
	run := mustCreate(t, t.TempDir(), time.Now())

	for _, node := range []string{"weft", "state", "events", "lock"} {
		f, err := run.CreateAttemptLog(node, 1)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		rec := store.AttemptRecord{Node: node, Attempt: 1}
		require.NoError(t, run.WriteAttemptRecord(rec))
	}

	entries, err := os.ReadDir(run.Dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		assert.False(t, e.IsDir(), "%s is a directory", e.Name())
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{
		"weft.1.log", "weft.1.json", "state.1.log", "state.1.json",
		"events.1.log", "events.1.json", "lock.1.log", "lock.1.json",
	}, names, "no node file takes the name of one of weft's own")
}

func TestOldLayoutRunsStayReadable(t *testing.T) {
	run := mustCreate(t, t.TempDir(), time.Now())
	oldDir := filepath.Join(run.Dir, "nodes", "build")
	require.NoError(t, os.MkdirAll(oldDir, 0o700))
	oldLog := filepath.Join(oldDir, "1.log")
	oldRecord := filepath.Join(oldDir, "1.json")
	require.NoError(t, os.WriteFile(oldLog, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(oldRecord, []byte("{}\n"), 0o600))

	assert.Equal(t, oldLog, run.AttemptLogPath("build", 1))
	assert.Equal(t, oldRecord, run.AttemptRecordPath("build", 1))
	assert.Equal(t, filepath.Join(run.Dir, "build.2.log"), run.AttemptLogPath("build", 2), "a resume writes new attempts flat")
	assert.Equal(t, filepath.Join(run.Dir, "build.2.json"), run.AttemptRecordPath("build", 2))
}

func TestCreateAttemptLog(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())

	f, err := run.CreateAttemptLog("build", 1)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	path := run.AttemptLogPath("build", 1)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteAttemptRecord(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	rec := store.AttemptRecord{
		Node:        "build",
		Attempt:     1,
		Command:     "make",
		Shell:       []string{"bash", "-c"},
		Dir:         ".",
		Interactive: false,
		State:       "failed",
		Reason:      "exit",
		ExitCode:    2,
		Signal:      "",
		Error:       "",
		Started:     time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		Finished:    time.Date(2026, 3, 4, 5, 6, 9, 0, time.UTC),
		Log:         "build.1.log",
	}

	require.NoError(t, run.WriteAttemptRecord(rec))

	path := run.AttemptRecordPath("build", 1)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var got store.AttemptRecord
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, rec, got)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"build.1.json"}, names)
}

func TestPrune(t *testing.T) {
	t.Parallel()

	t.Run("keeps the newest run directories", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		base := filepath.Join(root, ".weft", "runs", "deploy")
		runIDs := []string{
			"20260101T000000.000-aaaa",
			"20260102T000000.000-bbbb",
			"20260103T000000.000-cccc",
			"20260104T000000.000-dddd",
			"20260105T000000.000-eeee",
		}
		for _, id := range runIDs {
			require.NoError(t, os.MkdirAll(filepath.Join(base, id), 0o700))
		}
		require.NoError(t, os.MkdirAll(filepath.Join(base, "scratch"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "20260101T000000.000-ffff.txt"), nil, 0o600))

		require.NoError(t, store.Prune(root, "deploy", 2))

		entries, err := os.ReadDir(base)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.Equal(t, []string{
			"20260101T000000.000-ffff.txt",
			"20260104T000000.000-dddd",
			"20260105T000000.000-eeee",
			"scratch",
		}, names)
	})

	t.Run("missing workflow runs dir is not an error", func(t *testing.T) {
		t.Parallel()

		assert.NoError(t, store.Prune(t.TempDir(), "absent", 3))
	})
}

func TestWriteEvents(t *testing.T) {
	t.Parallel()

	run := mustCreate(t, t.TempDir(), time.Now())
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	ch := make(chan events.Event, 4)
	ch <- events.RunStarted{Workflow: "deploy", RunID: run.ID, RunDir: run.Dir, At: at}
	ch <- events.NodeChanged{
		Node:     "build",
		State:    events.Running,
		Attempt:  1,
		ExitCode: 0,
		Err:      "",
		Reason:   "",
		Started:  at,
		Finished: time.Time{},
		At:       at,
	}
	ch <- events.NodeOutput{Node: "build", Line: "hello"}
	ch <- events.RunFinished{Success: true, At: at}
	close(ch)

	require.NoError(t, run.WriteEvents(ch))

	path := run.EventsPath()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 3)

	type line struct {
		Seq  int    `json:"seq"`
		Type string `json:"type"`
	}
	wantTypes := []string{"run_started", "node_changed", "run_finished"}
	for i, l := range lines {
		var got line
		require.NoError(t, json.Unmarshal([]byte(l), &got))
		assert.Equal(t, i+1, got.Seq)
		assert.Equal(t, wantTypes[i], got.Type)
	}
}
