package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/report"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

var t0 = time.Date(2026, 10, 25, 19, 12, 0, 0, time.UTC)

const nDeploy = "deploy"

func node(state events.State, attempt int, errText string, from, to time.Duration, log string) store.NodeState {
	ns := store.NodeState{
		State: state, Attempt: attempt, ExitCode: 0, Reason: "", Error: errText,
		Started: time.Time{}, Finished: time.Time{}, Log: log, PGID: 0,
	}
	if from >= 0 {
		ns.Started = t0.Add(from)
	}
	if to >= 0 {
		ns.Finished = t0.Add(to)
	}
	return ns
}

func newRun(t *testing.T, nodes map[string]store.NodeState) (*store.Run, *store.State) {
	t.Helper()
	run, err := store.Create(t.TempDir(), nDeploy, t0)
	require.NoError(t, err)
	st := store.NewState(run, "/repo/.weft/deploy.yml", t0)
	st.Updated = t0.Add(time.Minute)
	st.History = []store.Start{{At: t0, PID: 1, Mode: store.ModeRun, Nodes: nil, WorkflowHash: "h", GitHead: "", Forced: false}}
	st.Nodes = nodes
	return run, st
}

func loadWorkflow(t *testing.T) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.Parse([]byte(`
nodes:
  release: {run: x, label: Release}
  deploy: {run: y, needs: [release]}
  apply: {run: z, needs: [deploy]}
`))
	require.NoError(t, err)
	return wf
}

func TestStatus(t *testing.T) {
	t.Parallel()

	ok := node(events.Succeeded, 1, "", 0, time.Second, "")
	tests := []struct {
		name  string
		nodes map[string]store.NodeState
		want  report.Status
	}{
		{"all succeeded", map[string]store.NodeState{"a": ok, "b": node(events.Skipped, 1, "", 0, 1, "")}, report.Succeeded},
		{"excluded nodes do not fail it", map[string]store.NodeState{"a": ok, "b": node(events.Excluded, 0, "", -1, -1, "")}, report.Succeeded},
		{"a failure", map[string]store.NodeState{"a": node(events.Failed, 1, "exit code 1", 0, 1, ""), "b": node(events.Blocked, 1, "", -1, 0, "")}, report.Failed},
		{"a cancel", map[string]store.NodeState{"a": node(events.Canceled, 1, "", 0, 1, ""), "b": node(events.Blocked, 1, "", -1, 0, "")}, report.Canceled},
		{"failure beats cancel", map[string]store.NodeState{"a": node(events.Canceled, 1, "", 0, 1, ""), "b": node(events.Failed, 1, "", 0, 1, "")}, report.Failed},
		{"left running by a dead weft", map[string]store.NodeState{"a": ok, "b": node(events.Running, 1, "", 0, -1, "")}, report.Interrupted},
		{"never started", map[string]store.NodeState{"a": node(events.Pending, 1, "", -1, -1, "")}, report.Interrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, st := newRun(t, tt.nodes)
			assert.Equal(t, tt.want, report.Build(run, st, nil).Status)
		})
	}
}

func TestBuildRunningWhileLocked(t *testing.T) {
	t.Parallel()

	run, st := newRun(t, map[string]store.NodeState{"a": node(events.Running, 1, "", 0, -1, "")})
	lock, err := run.Lock()
	require.NoError(t, err)
	defer lock.Release()

	r := report.Build(run, st, nil)
	assert.Equal(t, report.Running, r.Status)
	assert.Equal(t, os.Getpid(), r.PID)
}

func TestBuildFollowsWorkflowOrder(t *testing.T) {
	t.Parallel()

	run, st := newRun(t, map[string]store.NodeState{
		"apply":   node(events.Blocked, 1, "blocked: deploy failed", -1, 90*time.Second, ""),
		nDeploy:   node(events.Failed, 2, "exit code 1", 60*time.Second, 90*time.Second, "nodes/deploy/2.log"),
		"release": node(events.Succeeded, 1, "", 0, 50*time.Second, "nodes/release/1.log"),
		"gone":    node(events.Succeeded, 1, "", 0, time.Second, ""),
	})
	r := report.Build(run, st, loadWorkflow(t))

	var ids []string
	for _, n := range r.Nodes {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"release", nDeploy, "apply", "gone"}, ids, "a node the file no longer has comes last")
	assert.Equal(t, "Release", r.Nodes[0].Label)
	assert.Equal(t, filepath.Join(run.Dir, "nodes/deploy/2.log"), r.Nodes[1].Log)
	assert.Equal(t, int64(30000), r.Nodes[1].DurationMS)
	assert.Equal(t, "2 succeeded · 1 failed · 1 blocked", r.Progress())
	assert.Equal(t, report.Failed, r.Status)
}

func TestText(t *testing.T) {
	t.Parallel()

	run, st := newRun(t, map[string]store.NodeState{
		"release": node(events.Succeeded, 1, "", 0, 50*time.Second, "nodes/release/1.log"),
		nDeploy:   node(events.Failed, 2, "exit code 1", 60*time.Second, 90*time.Second, "nodes/deploy/2.log"),
		"apply":   node(events.Blocked, 1, "blocked: deploy failed", -1, 90*time.Second, ""),
	})
	require.NoError(t, os.MkdirAll(filepath.Join(run.Dir, "nodes", nDeploy), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run.Dir, "nodes/deploy/2.log"), []byte("one\ntwo\nthree\n"), 0o600))

	r := report.Build(run, st, loadWorkflow(t))
	r.AddLogTails(2)
	assert.Nil(t, r.Nodes[0].LogTail, "no tail for a node that succeeded")
	assert.Equal(t, []string{"two", "three"}, r.Nodes[1].LogTail)

	text := r.Text(nDeploy, true)
	for _, want := range []string{
		"deploy run " + run.ID + ": failed\n",
		"progress: 1 succeeded · 1 failed · 1 blocked\n",
		"release  succeeded  0:50\n",
		"deploy   failed     attempt 2  0:30  exit code 1\n",
		"apply    blocked    blocked: deploy failed\n",
		"deploy, attempt 2: failed\nlog " + filepath.Join(run.Dir, "nodes/deploy/2.log") + "\n  two\n  three\n",
		"weft logs deploy deploy --run " + run.ID,
		"weft run deploy --resume",
	} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, r.Text(nDeploy, false), "--resume", "--resume continues only the latest run")
}

func TestSummaryLeavesOutNodes(t *testing.T) {
	t.Parallel()

	run, st := newRun(t, map[string]store.NodeState{
		"release": node(events.Succeeded, 1, "", 0, 1, ""),
		nDeploy:   node(events.Failed, 1, "", 0, 1, ""),
	})
	r := report.Build(run, st, loadWorkflow(t))
	data, err := json.Marshal(r.Summary())
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.NotContains(t, got, "nodes")
	assert.Equal(t, []any{nDeploy}, got["unfinished"])
	assert.Equal(t, "failed", got["status"])
}

func TestTail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	lines, err := report.Tail(write("short", "a\nb\nc"), 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, lines)

	lines, err = report.Tail(write("empty", ""), 5)
	require.NoError(t, err)
	assert.Empty(t, lines)

	// The read window starts mid-line; that partial line is not a line.
	big := strings.Repeat("x", 300*1024) + "\nlast\n"
	lines, err = report.Tail(write("big", big), 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"last"}, lines)

	_, err = report.Tail(filepath.Join(dir, "missing"), 5)
	require.Error(t, err)
}
