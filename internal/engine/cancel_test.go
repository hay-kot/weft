package engine_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
)

func hang() behavior {
	b := ok()
	b.hang = true
	return b
}

func isState(node string, st events.State) func(events.Event) bool {
	return func(e events.Event) bool {
		nc, ok := e.(events.NodeChanged)
		return ok && nc.Node == node && nc.State == st
	}
}

// runAsync starts the engine and returns a channel with its summary.
func (h *harness) runAsync(t *testing.T) <-chan engine.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	done := make(chan engine.Summary, 1)
	go func() { done <- h.eng.Run(ctx) }()
	return done
}

func (h *harness) finishes() int {
	n := 0
	for _, e := range h.snapshot() {
		if isFinished(e) {
			n++
		}
	}
	return n
}

const branchesYAML = `
nodes:
  a: {run: a}
  b: {run: b}
  c: {run: c, needs: [a]}
  d: {run: d, needs: [b]}
`

func TestCancelNodeStopsOneBranch(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, branchesYAML, map[string]behavior{"a": hang(), "b": gated(gate)}, 0)
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))
	h.waitEvent(t, isState("b", events.Running))

	action, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	assert.Equal(t, plan.CancelRunning, action)
	h.waitEvent(t, isState("c", events.Blocked))
	assert.Equal(t, 0, h.finishes(), "the pass goes on while b runs")
	close(gate)

	s := <-done
	h.closeBus()
	<-h.done
	assert.False(t, s.Success, "a canceled node fails the run")
	assert.Equal(t, map[string]events.State{
		"a": events.Canceled, "b": events.Succeeded, "c": events.Blocked, "d": events.Succeeded,
	}, s.States)
	assert.Empty(t, h.fake.killed, "one cancel is graceful")
	assert.Equal(t, 1, h.finishes())

	var rec store.AttemptRecord
	readJSON(t, h.run.AttemptRecordPath("a", 1), &rec)
	assert.Equal(t, string(events.Canceled), rec.State)
	assert.Equal(t, events.ReasonCanceled, rec.Reason)
	assert.True(t, logHas(h.logBuf.String(), `"message":"node cancel requested"`, `"node":"a"`, `"attempt":1`, `"action":"cancel_running"`),
		"weft.log: %s", h.logBuf.String())
}

// logHas reports whether one log line contains every part.
func logHas(log string, parts ...string) bool {
	for line := range strings.SplitSeq(log, "\n") {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

func TestCancelNodeTwiceKills(t *testing.T) {
	trap := hang()
	trap.trapINT = true
	h := newHarness(t, "nodes:\n  a: {run: a}\n", map[string]behavior{"a": trap}, 0)
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))

	first, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	assert.Equal(t, plan.CancelRunning, first)
	second, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	assert.Equal(t, plan.KillRunning, second)

	s := <-done
	assert.Equal(t, events.Canceled, s.States["a"])
	assert.Equal(t, []string{"a"}, h.fake.killed)
}

func TestCancelNodeBeforeStart(t *testing.T) {
	gate := make(chan struct{})
	yml := `
nodes:
  a: {run: a}
  b: {run: b, needs: [a]}
  c: {run: c, needs: [b]}
`
	h := newHarness(t, yml, map[string]behavior{"a": gated(gate)}, 0)
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))

	action, err := h.eng.CancelNode("b")
	require.NoError(t, err)
	assert.Equal(t, plan.CancelUnstarted, action)
	h.waitEvent(t, isState("c", events.Blocked))
	close(gate)

	s := <-done
	assert.Equal(t, map[string]events.State{"a": events.Succeeded, "b": events.Canceled, "c": events.Blocked}, s.States)
	assert.Equal(t, []string{"a"}, h.fake.started)
	var rec store.AttemptRecord
	readJSON(t, h.run.AttemptRecordPath("b", lastChange(h.snapshot(), "b").Attempt), &rec)
	assert.Equal(t, events.ReasonCanceled, rec.Reason)
}

func TestCancelNodeWaitingForTheTerminal(t *testing.T) {
	gate := make(chan struct{})
	yml := `
nodes:
  x: {run: x}
  y: {run: y}
  prompt: {run: prompt, needs: [y], interactive: true}
`
	h := newHarness(t, yml, map[string]behavior{"x": gated(gate)}, 0)
	done := h.runAsync(t)
	h.waitEvent(t, isState("prompt", events.Waiting))

	action, err := h.eng.CancelNode("prompt")
	require.NoError(t, err)
	assert.Equal(t, plan.CancelUnstarted, action)
	close(gate)

	s := <-done
	assert.Equal(t, events.Canceled, s.States["prompt"])
	assert.Equal(t, events.Succeeded, s.States["x"])
	assert.Equal(t, 0, h.lender.calls, "the canceled node never got the terminal")
}

func TestCancelNodeRefusesFinishedNodes(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, branchesYAML, map[string]behavior{"b": gated(gate)}, 0)
	done := h.runAsync(t)
	h.waitEvent(t, isState("c", events.Succeeded))

	_, err := h.eng.CancelNode("a")
	require.EqualError(t, err, "can't cancel a: it is succeeded")
	_, err = h.eng.CancelNode("nope")
	require.EqualError(t, err, "can't cancel nope: no such node")
	close(gate)

	s := <-done
	assert.True(t, s.Success, "%v", s.States)
	_, err = h.eng.CancelNode("b")
	require.ErrorIs(t, err, engine.ErrClosed)
}

func TestCanceledNodeReruns(t *testing.T) {
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n  b: {run: b, needs: [a]}\n", map[string]behavior{"a": hang()},
		func(o *engine.Options) { o.Reruns = true })
	h.fake.perAttempt["a"] = map[int]behavior{2: ok()}
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))

	_, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	h.waitEvent(t, isFinished)
	_, err = h.eng.CancelNode("a")
	require.EqualError(t, err, "can't cancel a: it is canceled", "between passes nothing runs")

	require.NoError(t, h.eng.Rerun("a", true, emptyStart()))
	require.Eventually(t, func() bool { return h.finishes() == 2 }, 5*time.Second, 5*time.Millisecond)
	h.eng.Close()
	s := <-done
	assert.True(t, s.Success, "%v", s.States)
	assert.Equal(t, 2, lastChange(h.snapshot(), "a").Attempt)
}

// A node killed with CancelNode can be killed again by a kill of the pass
// before its result arrives.
func TestNodeKillThenPassKill(t *testing.T) {
	release := make(chan struct{})
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n", nil, func(o *engine.Options) {
		o.Runner = runnerFunc(func(_ context.Context, s runner.Spec) runner.Result {
			<-s.Kill
			<-release
			return result(runner.ReasonCanceled, -1, "killed")
		})
	})
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))

	_, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	action, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	assert.Equal(t, plan.KillRunning, action)
	h.eng.Kill()
	_, err = h.eng.CancelNode("a")
	require.NoError(t, err, "a third cancel is a kill again, not a panic")
	close(release)

	s := <-done
	assert.Equal(t, events.Canceled, s.States["a"])
}

// After Ctrl-C has sent SIGINT to a node that traps it, x on that node is
// the second request to stop it, so it kills instead of canceling again.
func TestCancelNodeAfterPassCancelKills(t *testing.T) {
	interrupted := make(chan struct{})
	var killed atomic.Bool
	h := newHarnessWith(t, "nodes:\n  a: {run: a}\n", nil, func(o *engine.Options) {
		o.Runner = runnerFunc(func(ctx context.Context, s runner.Spec) runner.Result {
			<-ctx.Done()
			close(interrupted)
			<-s.Kill
			killed.Store(true)
			return result(runner.ReasonCanceled, -1, "killed")
		})
	})
	done := h.runAsync(t)
	h.waitEvent(t, isState("a", events.Running))

	h.eng.Cancel()
	<-interrupted
	action, err := h.eng.CancelNode("a")
	require.NoError(t, err)
	assert.Equal(t, plan.KillRunning, action)

	select {
	case s := <-done:
		assert.Equal(t, events.Canceled, s.States["a"])
		assert.True(t, killed.Load(), "the node got the kill")
	case <-time.After(5 * time.Second):
		h.eng.Kill()
		assert.FailNow(t, "the node was not killed")
	}
}
