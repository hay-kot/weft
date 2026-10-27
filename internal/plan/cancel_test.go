package plan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
)

func TestCancel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		states   map[string]events.State
		node     string
		stopping bool
		want     plan.CancelAction
		err      string
	}{
		{
			name:   "running node shuts down gracefully",
			states: states(map[string]events.State{nDBBackup: events.Running}),
			node:   nDBBackup,
			want:   plan.CancelRunning,
		},
		{
			name:     "running node already stopping is killed",
			states:   states(map[string]events.State{nDBBackup: events.Running}),
			node:     nDBBackup,
			stopping: true,
			want:     plan.KillRunning,
		},
		{
			name:   "pending node",
			states: states(map[string]events.State{nDeploy: events.Pending}),
			node:   nDeploy,
			want:   plan.CancelUnstarted,
		},
		{
			name:   "ready node waiting for a slot",
			states: states(map[string]events.State{nRelease: events.Ready}),
			node:   nRelease,
			want:   plan.CancelUnstarted,
		},
		{
			name:   "interactive node waiting for the terminal",
			states: states(map[string]events.State{nTofu: events.Waiting}),
			node:   nTofu,
			want:   plan.CancelUnstarted,
		},
		{
			name:   "succeeded node",
			states: states(nil),
			node:   nGuard,
			err:    "can't cancel guard: it is succeeded",
		},
		{
			name:   "failed node",
			states: states(map[string]events.State{nDeploy: events.Failed}),
			node:   nDeploy,
			err:    "can't cancel deploy: it is failed",
		},
		{
			name:   "canceled node",
			states: states(map[string]events.State{nSide: events.Canceled}),
			node:   nSide,
			err:    "can't cancel side: it is canceled",
		},
		{
			name:   "blocked node",
			states: states(map[string]events.State{nTofu: events.Blocked}),
			node:   nTofu,
			err:    "can't cancel tofu-apply: it is blocked",
		},
		{
			name:   "skipped node",
			states: states(map[string]events.State{nRelease: events.Skipped}),
			node:   nRelease,
			err:    "can't cancel release: it is skipped",
		},
		{
			name:   "excluded node",
			states: states(map[string]events.State{nDeploy: events.Excluded}),
			node:   nDeploy,
			err:    "can't cancel deploy: it is excluded",
		},
		{
			name:   "unknown node",
			states: states(nil),
			node:   "nope",
			err:    "can't cancel nope: no such node",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := plan.Cancel(deployWorkflow(t), tt.states, tt.node, tt.stopping)
			if tt.err != "" {
				require.EqualError(t, err, tt.err)
				var ce *plan.CancelError
				require.ErrorAs(t, err, &ce)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
