package tui

import (
	"bytes"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
)

// These run the Model in a real Bubble Tea program with scripted events.
// The Update tests cover behavior; these check that the pieces hold
// together end to end.

func newProgram(t *testing.T) (*teatest.TestModel, *fakeInterrupt) {
	t.Helper()
	m, fi := newModel(t, 160, 45)
	return teatest.NewTestModel(t, m, teatest.WithInitialTermSize(160, 45)), fi
}

func waitOutput(t *testing.T, tm *teatest.TestModel, want string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(want)) },
		teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

func finalModel(t *testing.T, tm *teatest.TestModel) Model {
	t.Helper()
	m, ok := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(Model)
	require.True(t, ok)
	return m
}

func TestProgramRunsToTheEndAndQuits(t *testing.T) {
	tm, fi := newProgram(t)
	tm.Send(checksPassed())
	waitOutput(t, tm, "building binaries")
	tm.Send(deployFailed())
	waitOutput(t, tm, "failing on request")
	tm.Send(press("q"))

	m := finalModel(t, tm)
	assert.True(t, m.rs.done)
	assert.Equal(t, nDeploy, m.graph.selected)
	assert.Zero(t, fi.calls)
}

func TestProgramCancelThenQuitAfterTheRunEnds(t *testing.T) {
	tm, fi := newProgram(t)
	tm.Send(checksPassed())
	waitOutput(t, tm, "building binaries")
	tm.Send(press("ctrl+c"))
	waitOutput(t, tm, "canceling")
	tm.Send(ev(notRun(nRelease, events.Canceled, "canceled (interrupt)", 4), finished(false, 4)))
	waitOutput(t, tm, "canceled")
	tm.Send(press("q"))

	m := finalModel(t, tm)
	assert.Equal(t, 1, fi.calls)
	assert.Equal(t, phaseCanceled, runPhase(m.rs, m.cancelLevel))
}

func TestProgramLendsTheTerminal(t *testing.T) {
	tm, _ := newProgram(t)
	tm.Send(ev(started(), running(nTofu, 0)))
	waitOutput(t, tm, "tofu-apply")

	ran := false
	reply := make(chan error, 1)
	tm.Send(lendMsg{fn: func() { ran = true }, reply: reply})
	select {
	case err := <-reply:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the terminal was never handed back")
	}
	assert.True(t, ran)
	waitOutput(t, tm, "has the terminal")

	tm.Send(ev(ended(nTofu, events.Succeeded, "", 0, 1), finished(true, 1)))
	tm.Send(press("q"))
	assert.True(t, finalModel(t, tm).rs.success)
}
