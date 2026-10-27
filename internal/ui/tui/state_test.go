package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/workflow"
)

// deployYAML has the shape of the stub fixture in testdata/workflows.
const deployYAML = `
nodes:
  guard: {run: x}
  go-check: {run: x, needs: [guard]}
  ui-check: {run: x, needs: [guard]}
  db-backup: {run: x, needs: [guard]}
  release: {run: x, needs: [go-check, ui-check, db-backup]}
  deploy: {run: x, needs: [release]}
  tofu-apply: {run: x, needs: [deploy], interactive: true}
`

func deployWorkflow(t *testing.T) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.Parse([]byte(deployYAML))
	require.NoError(t, err)
	wf.Name = workflowName
	return wf
}

var t0 = time.Date(2026, 10, 25, 19, 12, 0, 0, time.UTC)

func TestIcon(t *testing.T) {
	tests := map[events.State]string{
		events.Pending:   "○",
		events.Ready:     "○",
		events.Running:   "●",
		events.Waiting:   "◆",
		events.Succeeded: "✓",
		events.Skipped:   "✓",
		events.Failed:    "✗",
		events.Blocked:   "⊘",
		events.Canceled:  "⊘",
	}
	for st, want := range tests {
		assert.Equal(t, want, icon(st), string(st))
	}
}

func TestSpinnerFrameIsAFunctionOfTime(t *testing.T) {
	assert.Equal(t, spinnerFrames[0], spinnerFrame(t0))
	assert.Equal(t, spinnerFrames[1], spinnerFrame(t0.Add(spinnerInterval)))
	assert.Equal(t, spinnerFrames[0], spinnerFrame(t0.Add(spinnerInterval*time.Duration(len(spinnerFrames)))))
}

func TestCellFor(t *testing.T) {
	n := &workflow.Node{
		ID: nRelease, Label: "", Run: "x", Needs: nil, Dir: "", Env: nil, Shell: nil,
		Interactive: false, Timeout: 0, KillTimeout: 0, ContinueOnError: false,
	}
	tests := []struct {
		name string
		st   nodeStatus
		want layout.Cell
	}{
		{
			"pending has no duration",
			nodeStatus{State: events.Pending, Attempt: 1, ExitCode: 0, Err: "", Reason: "", Started: time.Time{}, Finished: time.Time{}},
			layout.Cell{Prefix: "○ ", Label: nRelease, Suffix: ""},
		},
		{
			"running counts up to now",
			nodeStatus{State: events.Running, Attempt: 1, ExitCode: 0, Err: "", Reason: "", Started: t0, Finished: time.Time{}},
			layout.Cell{Prefix: "● ", Label: nRelease, Suffix: " 1:31"},
		},
		{
			"finished shows its run time",
			nodeStatus{State: events.Failed, Attempt: 1, ExitCode: 1, Err: "exit code 1", Reason: "exit", Started: t0, Finished: t0.Add(22 * time.Second)},
			layout.Cell{Prefix: "✗ ", Label: nRelease, Suffix: " 0:22"},
		},
		{
			"start error has no duration",
			nodeStatus{State: events.Failed, Attempt: 1, ExitCode: -1, Err: "x", Reason: "start_error", Started: t0, Finished: t0},
			layout.Cell{Prefix: "✗ ", Label: nRelease, Suffix: ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cellFor(n, &tt.st, t0.Add(91*time.Second)))
		})
	}
}

func TestAutoSelect(t *testing.T) {
	tests := []struct {
		name     string
		states   map[string]events.State
		selected string
		changed  string
		want     string
	}{
		{
			"a start takes selection from an idle node",
			map[string]events.State{nGuard: events.Succeeded, nGoCheck: events.Running},
			nGuard, nGoCheck, nGoCheck,
		},
		{
			"a start does not take selection from a running node",
			map[string]events.State{nGoCheck: events.Running, nUICheck: events.Running},
			nGoCheck, nUICheck, nGoCheck,
		},
		{
			"a failure takes selection from a running node",
			map[string]events.State{nGoCheck: events.Running, nUICheck: events.Failed},
			nGoCheck, nUICheck, nUICheck,
		},
		{
			"a later failure keeps the first one selected",
			map[string]events.State{nGoCheck: events.Failed, nUICheck: events.Failed},
			nGoCheck, nUICheck, nGoCheck,
		},
		{
			"a start does not take selection from a failure",
			map[string]events.State{nGoCheck: events.Failed, nDBBackup: events.Running},
			nGoCheck, nDBBackup, nGoCheck,
		},
		{
			"when the selected node succeeds selection moves to running work",
			map[string]events.State{nGoCheck: events.Succeeded, nUICheck: events.Running, nDBBackup: events.Running},
			nGoCheck, nGoCheck, nUICheck,
		},
		{
			"when the last node succeeds selection stays",
			map[string]events.State{nTofu: events.Succeeded},
			nTofu, nTofu, nTofu,
		},
		{
			"another node finishing leaves selection alone",
			map[string]events.State{nGoCheck: events.Running, nUICheck: events.Succeeded},
			nGoCheck, nUICheck, nGoCheck,
		},
		{
			"an interactive node waiting for the terminal takes selection",
			map[string]events.State{nDeploy: events.Succeeded, nTofu: events.Waiting},
			nDeploy, nTofu, nTofu,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRunState(deployWorkflow(t), testRun(), nil, 10)
			for id, st := range tt.states {
				rs.nodes[id].State = st
			}
			assert.Equal(t, tt.want, rs.autoSelect(tt.selected, tt.changed))
		})
	}
}

func TestRunStateElapsedAndCounts(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 10)
	assert.Equal(t, time.Duration(0), rs.elapsed(t0), "not started")

	rs.started = t0
	assert.Equal(t, 30*time.Second, rs.elapsed(t0.Add(30*time.Second)))
	rs.done, rs.finished = true, t0.Add(40*time.Second)
	assert.Equal(t, 40*time.Second, rs.elapsed(t0.Add(time.Hour)), "a finished run stops the clock")

	rs.nodes[nGuard].State = events.Succeeded
	rs.nodes[nGoCheck].State = events.Failed
	rs.nodes[nRelease].State = events.Running
	assert.Equal(t, 2, rs.finishedCount())
	assert.Equal(t, 4, rs.counts()[events.Pending])
}
