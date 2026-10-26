package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/store"
)

func ok() behavior {
	return behavior{exit: 0, hang: false, signal: "", output: "", delay: 0, trapINT: false, gate: nil}
}

func fail() behavior {
	b := ok()
	b.exit = 1
	return b
}

func gated(gate chan struct{}) behavior {
	b := ok()
	b.gate = gate
	return b
}

func nodeState(state events.State, attempt int) store.NodeState {
	return store.NodeState{
		State: state, Attempt: attempt, ExitCode: 0, Reason: "", Error: "",
		Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0,
	}
}

func emptyStart() store.Start {
	return store.Start{At: time.Time{}, PID: 0, Mode: "", Nodes: nil, WorkflowHash: "", GitHead: "", Forced: false}
}

// deployFailedState is an earlier run of deployYAML where deploy failed.
func deployFailedState(run *store.Run) *store.State {
	s := store.NewState(run, "", time.Now())
	for _, id := range []string{"guard", "go-check", "ui-check", "db-backup"} {
		s.Nodes[id] = nodeState(events.Succeeded, 1)
	}
	s.Nodes["release"] = store.NodeState{
		State: events.Succeeded, Attempt: 1, ExitCode: 0, Reason: "exit", Error: "",
		Started: time.Unix(100, 0), Finished: time.Unix(160, 0), Log: "nodes/release/1.log", PGID: 0,
	}
	s.Nodes["deploy"] = nodeState(events.Failed, 1)
	s.Nodes["tofu-apply"] = nodeState(events.Blocked, 1)
	return s
}

func lastChange(evs []events.Event, node string) events.NodeChanged {
	var out events.NodeChanged
	for _, e := range evs {
		if nc, ok := e.(events.NodeChanged); ok && nc.Node == node {
			out = nc
		}
	}
	return out
}

func TestResumeRunsOnlyWhatDidNotSucceed(t *testing.T) {
	h := newHarnessWith(t, deployYAML, nil, func(o *engine.Options) {
		prior := deployFailedState(o.Run)
		p := plan.Resume(o.Workflow, prior)
		o.Plan = &p
		o.State = store.NewState(o.Run, "", time.Now())
		o.State.History = []store.Start{emptyStart()}
	})
	s := h.runToEnd(t)

	assert.True(t, s.Success)
	assert.Equal(t, []string{"deploy", "tofu-apply"}, h.fake.started)
	assert.Equal(t, events.Skipped, s.States["release"])
	assert.Equal(t, events.Succeeded, s.States["deploy"])

	release := lastChange(h.events, "release")
	assert.Equal(t, events.Skipped, release.State)
	assert.Equal(t, 1, release.Attempt)
	assert.Equal(t, events.ReasonSkipped, release.Reason)
	assert.Equal(t, time.Unix(160, 0), release.Finished, "a skipped node keeps its earlier times")
	assert.Equal(t, 2, lastChange(h.events, "deploy").Attempt)
	assert.Equal(t, []events.State{events.Skipped}, h.transitions("guard"))

	st, err := h.run.LoadState()
	require.NoError(t, err)
	assert.Equal(t, events.Skipped, st.Nodes["release"].State)
	assert.Equal(t, "nodes/release/1.log", st.Nodes["release"].Log)
	deploy := st.Nodes["deploy"]
	assert.Equal(t, events.Succeeded, deploy.State)
	assert.Equal(t, 2, deploy.Attempt)
	assert.Equal(t, "nodes/deploy/2.log", deploy.Log)
	assert.Zero(t, deploy.PGID, "a finished node has no process group")
	assert.Len(t, st.History, 1)
	assert.FileExists(t, h.run.AttemptRecordPath("deploy", 2))
}

func TestExcludedNodesDoNotRunOrFail(t *testing.T) {
	yml := `
nodes:
  a: {run: a}
  b: {run: b, needs: [a]}
  c: {run: c}
`
	h := newHarnessWith(t, yml, nil, func(o *engine.Options) {
		p, err := plan.Only(o.Workflow, nil, []string{"a", "b"})
		require.NoError(t, err)
		o.Plan = &p
	})
	s := h.runToEnd(t)
	assert.True(t, s.Success)
	assert.Equal(t, events.Excluded, s.States["c"])
	assert.ElementsMatch(t, []string{"a", "b"}, h.fake.started)
	assert.Equal(t, events.ReasonExcluded, lastChange(h.events, "c").Reason)
	assert.Equal(t, 0, lastChange(h.events, "c").Attempt)
}

func TestStateFileTracksRunningNodes(t *testing.T) {
	gate := make(chan struct{})
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n", map[string]behavior{"a": gated(gate)}, func(o *engine.Options) {
		o.State = store.NewState(o.Run, "", time.Now())
	})
	h.fake.onStart = func(string) {}
	// The fake runner never calls Started, so pgid stays zero; what this
	// checks is that state.json says running while the node runs.
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(t.Context()) }()
	require.Eventually(t, func() bool {
		st, err := h.run.LoadState()
		return err == nil && st.Nodes["a"].State == events.Running
	}, 5*time.Second, 5*time.Millisecond)
	close(gate)
	s := <-done
	assert.True(t, s.Success)
	st, err := h.run.LoadState()
	require.NoError(t, err)
	assert.Equal(t, events.Succeeded, st.Nodes["a"].State)
	assert.False(t, st.Updated.Before(st.Created))
}

const rerunYAML = `
nodes:
  a: {run: a}
  b: {run: b, needs: [a]}
  x: {run: x}
`

func isFinished(e events.Event) bool { _, ok := e.(events.RunFinished); return ok }

func TestRerunAfterThePassEnds(t *testing.T) {
	h := newHarnessWith(t, rerunYAML, map[string]behavior{"a": fail()}, func(o *engine.Options) {
		o.Reruns = true
		o.State = store.NewState(o.Run, "", time.Now())
	})
	h.fake.perAttempt["a"] = map[int]behavior{2: ok()}
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(t.Context()) }()

	h.waitEvent(t, isFinished)
	assert.Equal(t, 0, h.eng.Interrupt(), "no pass is active between passes")

	err := h.eng.Rerun("b", false, emptyStart())
	require.EqualError(t, err, "can't rerun b: it needs a, which is failed")

	require.NoError(t, h.eng.Rerun("a", true, emptyStart()))
	h.waitEvent(t, func(e events.Event) bool { rs, ok := e.(events.RunStarted); return ok && rs.Pass == 2 })
	require.Eventually(t, func() bool {
		evs := h.snapshot()
		n := 0
		for _, e := range evs {
			if isFinished(e) {
				n++
			}
		}
		return n == 2
	}, 5*time.Second, 5*time.Millisecond)

	h.eng.Close()
	s := <-done
	assert.True(t, s.Success, "%v", s.States)
	assert.Equal(t, []string{"a", "x", "a", "b"}, sortedFirstTwo(h.fake.started))
	assert.Equal(t, 2, lastChange(h.snapshot(), "a").Attempt)
	assert.Equal(t, 2, lastChange(h.snapshot(), "b").Attempt, "b was blocked in attempt 1")
	require.ErrorIs(t, h.eng.Rerun("a", false, emptyStart()), engine.ErrClosed)

	st, err := h.run.LoadState()
	require.NoError(t, err)
	require.Len(t, st.History, 1)
	assert.Equal(t, store.ModeRerun, st.History[0].Mode)
	assert.Equal(t, []string{"a"}, st.History[0].Nodes)
}

// sortedFirstTwo sorts the first two starts, which run in parallel.
func sortedFirstTwo(s []string) []string {
	if len(s) < 2 {
		return s
	}
	return append(sortedCopy(s[:2]), s[2:]...)
}

func TestRerunDuringAPassJoinsIt(t *testing.T) {
	gate := make(chan struct{})
	h := newHarnessWith(t, rerunYAML, map[string]behavior{"a": fail(), "x": gated(gate)}, func(*engine.Options) {})
	h.fake.perAttempt["a"] = map[int]behavior{2: ok()}
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(t.Context()) }()

	h.waitEvent(t, func(e events.Event) bool {
		nc, ok := e.(events.NodeChanged)
		return ok && nc.Node == "a" && nc.State == events.Failed
	})
	require.NoError(t, h.eng.Rerun("a", false, emptyStart()))
	h.waitEvent(t, func(e events.Event) bool {
		nc, ok := e.(events.NodeChanged)
		return ok && nc.Node == "a" && nc.State == events.Succeeded
	})
	close(gate)
	s := <-done
	h.closeBus()
	<-h.done

	assert.False(t, s.Success, "b stayed blocked: a single rerun leaves downstream alone")
	assert.Equal(t, events.Succeeded, s.States["a"])
	assert.Equal(t, events.Blocked, s.States["b"])
	finishes := 0
	for _, e := range h.snapshot() {
		if isFinished(e) {
			finishes++
		}
	}
	assert.Equal(t, 1, finishes, "the rerun joined the running pass")
}

func TestRerunRefusedWhileCanceling(t *testing.T) {
	gate := make(chan struct{})
	h := newHarnessWith(t, rerunYAML, map[string]behavior{
		"a": fail(),
		"x": {exit: 0, hang: true, signal: "", output: "", delay: 0, trapINT: true, gate: nil},
	}, func(*engine.Options) {})
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(t.Context()) }()
	h.waitEvent(t, func(e events.Event) bool {
		nc, ok := e.(events.NodeChanged)
		return ok && nc.Node == "a" && nc.State == events.Failed
	})
	assert.Equal(t, 1, h.eng.Interrupt())
	err := h.eng.Rerun("a", false, emptyStart())
	require.ErrorIs(t, err, engine.ErrCanceling)
	assert.Equal(t, 2, h.eng.Interrupt())
	close(gate)
	<-done
}

func TestCancelDoesNotCarryIntoTheNextPass(t *testing.T) {
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n", map[string]behavior{
		"a": {exit: 0, hang: true, signal: "", output: "", delay: 0, trapINT: false, gate: nil},
	}, func(o *engine.Options) { o.Reruns = true })
	h.fake.perAttempt["a"] = map[int]behavior{2: ok()}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(ctx) }()

	h.waitEvent(t, func(e events.Event) bool {
		nc, ok := e.(events.NodeChanged)
		return ok && nc.State == events.Running
	})
	require.Equal(t, 1, h.eng.Interrupt())
	h.waitEvent(t, isFinished)

	require.NoError(t, h.eng.Rerun("a", false, emptyStart()))
	h.eng.Close()
	s := <-done
	assert.Equal(t, events.Succeeded, s.States["a"], "the rerun ran instead of being canceled at once")
}

func TestRunContextStopsReruns(t *testing.T) {
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n", nil, func(o *engine.Options) { o.Reruns = true })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan engine.Summary)
	go func() { done <- h.eng.Run(ctx) }()
	h.waitEvent(t, isFinished)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
	err := h.eng.Rerun("a", false, emptyStart())
	require.ErrorIs(t, err, engine.ErrClosed)
}
