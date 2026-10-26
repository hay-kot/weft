package plan_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
)

func states(over map[string]events.State) map[string]events.State {
	out := map[string]events.State{
		nGuard: events.Succeeded, nGoCheck: events.Succeeded, nUICheck: events.Succeeded,
		nDBBackup: events.Succeeded, nRelease: events.Succeeded, nDeploy: events.Succeeded,
		nTofu: events.Succeeded, nSide: events.Succeeded,
	}
	maps.Copy(out, over)
	return out
}

func TestRerun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		states     map[string]events.State
		node       string
		downstream bool
		want       []string
		err        string
	}{
		{
			name:   "failed node with satisfied needs",
			states: states(map[string]events.State{nDeploy: events.Failed, nTofu: events.Blocked}),
			node:   nDeploy,
			want:   []string{nDeploy},
		},
		{
			name:       "with downstream resets the blocked nodes too",
			states:     states(map[string]events.State{nDeploy: events.Failed, nTofu: events.Blocked}),
			node:       nDeploy,
			downstream: true,
			want:       []string{nDeploy, nTofu},
		},
		{
			name:       "downstream of a fan-out root, in declaration order",
			states:     states(nil),
			node:       nGuard,
			downstream: true,
			want:       []string{nGuard, nGoCheck, nUICheck, nDBBackup, nRelease, nDeploy, nTofu},
		},
		{
			name:   "skipped and excluded nodes can rerun",
			states: states(map[string]events.State{nRelease: events.Skipped, nDeploy: events.Excluded}),
			node:   nDeploy,
			want:   []string{nDeploy},
		},
		{
			name:   "canceled node",
			states: states(map[string]events.State{nSide: events.Canceled}),
			node:   nSide,
			want:   []string{nSide},
		},
		{
			name:   "need failed with continue_on_error still satisfies",
			states: states(map[string]events.State{nDBBackup: events.Failed, nRelease: events.Failed}),
			node:   nRelease,
			want:   []string{nRelease},
		},
		{
			name:       "pending downstream is left to wait",
			states:     states(map[string]events.State{nRelease: events.Failed, nDeploy: events.Pending, nTofu: events.Pending}),
			node:       nRelease,
			downstream: true,
			want:       []string{nRelease},
		},
		{
			name:   "running node",
			states: states(map[string]events.State{nDeploy: events.Running}),
			node:   nDeploy,
			err:    "can't rerun deploy: it is running",
		},
		{
			name:   "pending node",
			states: states(map[string]events.State{nDeploy: events.Pending}),
			node:   nDeploy,
			err:    "can't rerun deploy: it is pending",
		},
		{
			name:   "need failed",
			states: states(map[string]events.State{nDeploy: events.Failed, nTofu: events.Blocked}),
			node:   nTofu,
			err:    "can't rerun tofu-apply: it needs deploy, which is failed",
		},
		{
			name:   "need being rerun",
			states: states(map[string]events.State{nRelease: events.Running}),
			node:   nDeploy,
			err:    "can't rerun deploy: it needs release, which is running",
		},
		{
			name:       "downstream still running",
			states:     states(map[string]events.State{nTofu: events.Running}),
			node:       nRelease,
			downstream: true,
			err:        "can't rerun release: tofu-apply, downstream of it, is running",
		},
		{
			name:       "downstream waiting for the terminal",
			states:     states(map[string]events.State{nTofu: events.Waiting}),
			node:       nDeploy,
			downstream: true,
			err:        "can't rerun deploy: tofu-apply, downstream of it, is waiting",
		},
		{
			name:   "running downstream does not stop a single rerun",
			states: states(map[string]events.State{nTofu: events.Running}),
			node:   nDeploy,
			want:   []string{nDeploy},
		},
		{
			name:   "unknown node",
			states: states(nil),
			node:   "nope",
			err:    "can't rerun nope: no such node",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := plan.Rerun(deployWorkflow(t), tt.states, tt.node, tt.downstream)
			if tt.err != "" {
				require.EqualError(t, err, tt.err)
				var re *plan.RerunError
				require.ErrorAs(t, err, &re)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
