package plan_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// The fixture's shape: guard fans out to three checks, which fan into
// release, then deploy, then the interactive tofu-apply. side has no
// needs and nothing needs it.
const deployYAML = `
nodes:
  guard: {run: guard}
  go-check: {run: go-check, needs: [guard]}
  ui-check: {run: ui-check, needs: [guard]}
  db-backup: {run: db-backup, needs: [guard], continue_on_error: true}
  release: {run: release, needs: [go-check, ui-check, db-backup]}
  deploy: {run: deploy, needs: [release]}
  tofu-apply: {run: tofu-apply, needs: [deploy], interactive: true}
  side: {run: side}
`

func deployWorkflow(t *testing.T) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.Parse([]byte(deployYAML))
	require.NoError(t, err)
	wf.Name = nDeploy
	return wf
}

func ns(state events.State, attempt int) store.NodeState {
	return store.NodeState{
		State: state, Attempt: attempt, ExitCode: 0, Reason: "", Error: "",
		Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0,
	}
}

// prior builds an earlier run's state from node states.
func prior(nodes map[string]store.NodeState) *store.State {
	return &store.State{
		Version: store.StateVersion, RunID: "20261025T191200.000-a3f9", Workflow: nDeploy, WorkflowPath: "",
		Created: time.Time{}, Updated: time.Time{}, History: nil, Nodes: nodes,
	}
}

// deployFailed is a run of the deploy fixture where deploy failed, so
// tofu-apply was blocked.
func deployFailed() *store.State {
	return prior(map[string]store.NodeState{
		nGuard:    ns(events.Succeeded, 1),
		nGoCheck:  ns(events.Succeeded, 1),
		nUICheck:  ns(events.Succeeded, 1),
		nDBBackup: ns(events.Succeeded, 1),
		nRelease:  ns(events.Succeeded, 1),
		nDeploy:   ns(events.Failed, 1),
		nTofu:     ns(events.Blocked, 1),
		nSide:     ns(events.Succeeded, 1),
	})
}

// Node IDs of deployYAML.
const (
	nGuard    = "guard"
	nGoCheck  = "go-check"
	nUICheck  = "ui-check"
	nDBBackup = "db-backup"
	nRelease  = "release"
	nDeploy   = "deploy"
	nTofu     = "tofu-apply"
	nSide     = "side"
)

// Shapes of planned nodes: state/attempt.
const (
	skip1 = "skipped/1"
	pend1 = "pending/1"
	pend2 = "pending/2"
	excl0 = "excluded/0"
)

// shape is a plan reduced to state and attempt per node.
type shape map[string]string

func shapeOf(p plan.Plan) shape {
	out := shape{}
	for id, n := range p.Nodes {
		out[id] = string(n.State) + "/" + strconv.Itoa(n.Attempt)
	}
	return out
}

func TestFresh(t *testing.T) {
	t.Parallel()

	wf := deployWorkflow(t)
	p := plan.Fresh(wf)
	assert.Equal(t, store.ModeRun, p.Mode)
	assert.False(t, p.Continue)
	for _, n := range wf.Nodes {
		assert.Equal(t, pend1, shapeOf(p)[n.ID], n.ID)
	}
	assert.Len(t, p.Pending(wf), len(wf.Nodes))
}

func TestResume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		prior *store.State
		want  shape
	}{
		{
			name:  "deploy failed: rerun deploy and tofu-apply, skip the rest",
			prior: deployFailed(),
			want: shape{
				nGuard: skip1, nGoCheck: skip1, nUICheck: skip1, nDBBackup: skip1,
				nRelease: skip1, nDeploy: pend2, nTofu: pend2, nSide: skip1,
			},
		},
		{
			name: "weft killed mid-run: running reruns, never-started reuses its attempt",
			prior: prior(map[string]store.NodeState{
				nGuard:    ns(events.Skipped, 1),
				nGoCheck:  ns(events.Running, 2),
				nUICheck:  ns(events.Succeeded, 2),
				nDBBackup: ns(events.Failed, 1),
				nRelease:  ns(events.Pending, 2),
				nDeploy:   ns(events.Ready, 1),
				nTofu:     ns(events.Waiting, 1),
				nSide:     ns(events.Canceled, 3),
			}),
			want: shape{
				nGuard: skip1, nGoCheck: "pending/3", nUICheck: "skipped/2", nDBBackup: pend2,
				nRelease: pend2, nDeploy: pend1, nTofu: pend1, nSide: "pending/4",
			},
		},
		{
			name: "nodes added to the workflow since run as new; excluded nodes run",
			prior: prior(map[string]store.NodeState{
				nGuard:   ns(events.Succeeded, 1),
				nSide:    ns(events.Excluded, 0),
				"absent": ns(events.Succeeded, 1),
			}),
			want: shape{
				nGuard: skip1, nGoCheck: pend1, nUICheck: pend1, nDBBackup: pend1,
				nRelease: pend1, nDeploy: pend1, nTofu: pend1, nSide: pend1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := plan.Resume(deployWorkflow(t), tt.prior)
			assert.Equal(t, store.ModeResume, p.Mode)
			assert.True(t, p.Continue)
			assert.Equal(t, tt.want, shapeOf(p))
		})
	}
}

func TestResumeKeepsPriorForSkipped(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 25, 19, 12, 0, 0, time.UTC)
	release := store.NodeState{
		State: events.Succeeded, Attempt: 1, ExitCode: 0, Reason: "exit", Error: "",
		Started: at, Finished: at.Add(time.Minute), Log: "nodes/release/1.log", PGID: 0,
	}
	pr := deployFailed()
	pr.Nodes[nRelease] = release
	p := plan.Resume(deployWorkflow(t), pr)
	assert.Equal(t, release, p.Nodes[nRelease].Prior)
}

func TestResumeNothingLeft(t *testing.T) {
	t.Parallel()

	wf := deployWorkflow(t)
	all := map[string]store.NodeState{}
	for _, n := range wf.Nodes {
		all[n.ID] = ns(events.Succeeded, 1)
	}
	assert.Empty(t, plan.Resume(wf, prior(all)).Pending(wf))
}

func TestFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		prior *store.State
		from  string
		want  shape
		cont  bool
		err   string
	}{
		{
			name:  "from deploy takes release from the latest run",
			prior: deployFailed(),
			from:  nDeploy,
			want: shape{
				nGuard: skip1, nGoCheck: skip1, nUICheck: skip1, nDBBackup: skip1,
				nRelease: skip1, nDeploy: pend2, nTofu: pend2, nSide: skip1,
			},
			cont: true,
		},
		{
			name:  "from release reruns a node that succeeded",
			prior: deployFailed(),
			from:  nRelease,
			want: shape{
				nGuard: skip1, nGoCheck: skip1, nUICheck: skip1, nDBBackup: skip1,
				nRelease: pend2, nDeploy: pend2, nTofu: pend2, nSide: skip1,
			},
			cont: true,
		},
		{
			name:  "from a root needs nothing earlier: a new run",
			prior: deployFailed(),
			from:  nGuard,
			want: shape{
				nGuard: pend1, nGoCheck: pend1, nUICheck: pend1, nDBBackup: pend1,
				nRelease: pend1, nDeploy: pend1, nTofu: pend1, nSide: excl0,
			},
			cont: false,
		},
		{
			name: "need failed in the latest run",
			prior: prior(map[string]store.NodeState{
				nRelease: ns(events.Failed, 1),
			}),
			from: nDeploy,
			err:  "deploy needs release, which is not selected and did not succeed in the latest run 20261025T191200.000-a3f9 (it is failed)",
		},
		{
			name:  "need never ran",
			prior: prior(map[string]store.NodeState{}),
			from:  nDeploy,
			err:   "deploy needs release, which is not selected and never ran in the latest run 20261025T191200.000-a3f9",
		},
		{
			name:  "no earlier run",
			prior: nil,
			from:  nDeploy,
			err:   "deploy needs release, which is not selected, and there is no earlier run to take its result from",
		},
		{
			name:  "unknown node",
			prior: nil,
			from:  "nope",
			err:   `workflow deploy has no node "nope" (nodes: guard, go-check, ui-check, db-backup, release, deploy, tofu-apply, side)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := plan.From(deployWorkflow(t), tt.prior, tt.from)
			if tt.err != "" {
				require.EqualError(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, store.ModeFrom, p.Mode)
			assert.Equal(t, []string{tt.from}, p.Selection)
			assert.Equal(t, tt.cont, p.Continue)
			assert.Equal(t, tt.want, shapeOf(p))
		})
	}
}

func TestOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		prior *store.State
		only  []string
		want  shape
		cont  bool
		err   string
	}{
		{
			name:  "only deploy: tofu-apply is left out",
			prior: deployFailed(),
			only:  []string{nDeploy},
			want: shape{
				nGuard: skip1, nGoCheck: skip1, nUICheck: skip1, nDBBackup: skip1,
				nRelease: skip1, nDeploy: pend2, nTofu: "excluded/1", nSide: skip1,
			},
			cont: true,
		},
		{
			name:  "everything but the interactive node, with no earlier run",
			prior: nil,
			only:  []string{nGuard, nGoCheck, nUICheck, nDBBackup, nRelease, nDeploy},
			want: shape{
				nGuard: pend1, nGoCheck: pend1, nUICheck: pend1, nDBBackup: pend1,
				nRelease: pend1, nDeploy: pend1, nTofu: excl0, nSide: excl0,
			},
			cont: false,
		},
		{
			name:  "self-contained selection ignores the latest run",
			prior: deployFailed(),
			only:  []string{nSide},
			want: shape{
				nGuard: excl0, nGoCheck: excl0, nUICheck: excl0, nDBBackup: excl0,
				nRelease: excl0, nDeploy: excl0, nTofu: excl0, nSide: pend1,
			},
			cont: false,
		},
		{
			name:  "need outside the selection without an earlier run",
			prior: nil,
			only:  []string{nRelease},
			err:   "release needs go-check, which is not selected, and there is no earlier run to take its result from",
		},
		{
			name:  "unknown node",
			prior: nil,
			only:  []string{nDeploy, "typo"},
			err:   `workflow deploy has no node "typo" (nodes: guard, go-check, ui-check, db-backup, release, deploy, tofu-apply, side)`,
		},
		{
			name:  "empty",
			prior: nil,
			only:  nil,
			err:   "no nodes given",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := plan.Only(deployWorkflow(t), tt.prior, tt.only)
			if tt.err != "" {
				require.EqualError(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, store.ModeOnly, p.Mode)
			assert.Equal(t, tt.only, p.Selection)
			assert.Equal(t, tt.cont, p.Continue)
			assert.Equal(t, tt.want, shapeOf(p))
		})
	}
}

func TestSatisfiesAndBlocks(t *testing.T) {
	t.Parallel()

	plain := &workflow.Node{ID: "a", ContinueOnError: false}
	soft := &workflow.Node{ID: "b", ContinueOnError: true}
	tests := []struct {
		node      *workflow.Node
		state     events.State
		satisfies bool
		blocks    bool
	}{
		{plain, events.Pending, false, false},
		{plain, events.Ready, false, false},
		{plain, events.Running, false, false},
		{plain, events.Waiting, false, false},
		{plain, events.Succeeded, true, false},
		{plain, events.Skipped, true, false},
		{plain, events.Failed, false, true},
		{soft, events.Failed, true, false},
		{plain, events.Canceled, false, true},
		{plain, events.Blocked, false, true},
		{plain, events.Excluded, false, true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.satisfies, plan.Satisfies(tt.node, tt.state), "satisfies %s %s", tt.node.ID, tt.state)
		assert.Equal(t, tt.blocks, plan.Blocks(tt.node, tt.state), "blocks %s %s", tt.node.ID, tt.state)
	}
}

func TestLost(t *testing.T) {
	t.Parallel()

	pr := prior(map[string]store.NodeState{
		nDeploy:  ns(events.Running, 2),
		nGoCheck: ns(events.Running, 1),
		nRelease: ns(events.Succeeded, 1),
	})
	assert.Equal(t, []string{nGoCheck, nDeploy}, plan.Lost(deployWorkflow(t), pr))
}

func TestDrift(t *testing.T) {
	t.Parallel()

	last := store.Start{
		At: time.Time{}, PID: 1, Mode: store.ModeRun, Nodes: nil,
		WorkflowHash: "hash1", GitHead: "0123456789abcdef", Forced: false,
	}
	tests := []struct {
		name string
		last store.Start
		hash string
		head string
		want []string
	}{
		{"unchanged", last, "hash1", "0123456789abcdef", nil},
		{"workflow changed", last, "hash2", "0123456789abcdef", []string{"the workflow file changed"}},
		{"head moved", last, "hash1", "fedcba9876543210", []string{"git HEAD moved from 0123456789ab to fedcba987654"}},
		{"both", last, "hash2", "", []string{"the workflow file changed", "git HEAD moved from 0123456789ab to none"}},
		{"no recorded start", store.Start{At: time.Time{}, PID: 0, Mode: "", Nodes: nil, WorkflowHash: "", GitHead: "", Forced: false}, "hash2", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, plan.Drift(tt.last, tt.hash, tt.head))
		})
	}
	err := &plan.DriftError{RunID: "r1", Reasons: []string{"the workflow file changed", "git HEAD moved from a to b"}}
	assert.EqualError(t, err, "since run r1 last started, the workflow file changed and git HEAD moved from a to b")
}
