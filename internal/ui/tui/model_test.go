package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// fakeInterrupt counts interrupts the way the real escalator does.
type fakeInterrupt struct{ calls int }

func (f *fakeInterrupt) Interrupt() int {
	f.calls++
	return f.calls
}

func newModel(t *testing.T, w, h int) (Model, *fakeInterrupt) {
	t.Helper()
	fi := &fakeInterrupt{calls: 0}
	m := New(Options{
		Workflow:  deployWorkflow(t),
		Run:       testRun(),
		ExitOnEnd: false,
		Interrupt: fi.Interrupt,
		Log:       zerolog.Nop(),
	})
	return send(t, m, tickMsg{at: t0}, tea.WindowSizeMsg{Width: w, Height: h}), fi
}

// send feeds msgs to Update and returns the model. Commands are dropped;
// tests that care about them call update directly.
func send(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		m, _ = update(t, m, msg)
	}
	return m
}

func update(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	got, ok := next.(Model)
	require.True(t, ok)
	return got, cmd
}

// runCmd runs cmd and every command a batch holds, and returns the
// messages they produce.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func hasQuit(msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func ev(e ...events.Event) eventsMsg { return eventsMsg{events: e} }

func started() events.RunStarted {
	return events.RunStarted{Workflow: workflowName, RunID: testRun().ID, RunDir: testRun().Dir, Pass: 1, At: t0}
}

func running(node string, sec float64) events.NodeChanged {
	return events.NodeChanged{
		Node: node, State: events.Running, Attempt: 1, ExitCode: 0, Err: "", Reason: "",
		Started: at(sec), Finished: time.Time{}, At: at(sec),
	}
}

func ended(node string, state events.State, errText string, from, to float64) events.NodeChanged {
	reason, code := events.ReasonExit, 0
	if state == events.Failed {
		code = 1
	}
	return events.NodeChanged{
		Node: node, State: state, Attempt: 1, ExitCode: code, Err: errText, Reason: reason,
		Started: at(from), Finished: at(to), At: at(to),
	}
}

func notRun(node string, state events.State, errText string, sec float64) events.NodeChanged {
	return events.NodeChanged{
		Node: node, State: state, Attempt: 1, ExitCode: -1, Err: errText, Reason: events.Reason(state),
		Started: time.Time{}, Finished: at(sec), At: at(sec),
	}
}

func output(node, line string) events.NodeOutput { return events.NodeOutput{Node: node, Line: line} }

func finished(success bool, sec float64) events.RunFinished {
	return events.RunFinished{Success: success, At: at(sec)}
}

// checksPassed scripts the fixture up to release running.
func checksPassed() eventsMsg {
	return ev(
		started(),
		running(nGuard, 0), output(nGuard, "on branch main (stub)"), ended(nGuard, events.Succeeded, "", 0, 0.2),
		running(nGoCheck, 0.2), running(nUICheck, 0.2), running(nDBBackup, 0.2),
		output(nGoCheck, "\x1b[36m•\x1b[0m go vet ./..."),
		ended(nUICheck, events.Succeeded, "", 0.2, 1.7),
		ended(nDBBackup, events.Succeeded, "", 0.2, 1.2),
		ended(nGoCheck, events.Succeeded, "", 0.2, 2.2),
		running(nRelease, 2.2), output(nRelease, "• building binaries  layout=dist"),
	)
}

// deployFailed scripts deploy failing and tofu-apply blocked.
func deployFailed() eventsMsg {
	return ev(
		output(nRelease, "release: tagged v0.0.0-stub"),
		ended(nRelease, events.Succeeded, "", 2.2, 3.0),
		running(nDeploy, 3.0), output(nDeploy, "deploy: rolling out v0.0.0-stub (stub)"),
		output(nDeploy, "deploy: failing on request (WEFT_FIXTURE_FAIL)"),
		ended(nDeploy, events.Failed, "exit code 1", 3.0, 3.5),
		notRun(nTofu, events.Blocked, "blocked: deploy failed", 3.5),
		finished(false, 3.5),
	)
}

func TestScriptedRunUpdatesState(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed())

	assert.Equal(t, events.Succeeded, m.rs.state(nGuard))
	assert.Equal(t, events.Running, m.rs.state(nRelease))
	assert.Equal(t, at(0), m.rs.started)
	assert.Equal(t, nRelease, m.graph.selected, "selection follows the run")
	assert.Equal(t, nRelease, m.pane.node)
	assert.Equal(t, "\x1b[36m•\x1b[0m go vet ./...", m.rs.logs[nGoCheck].at(0).Text)

	m = send(t, m, deployFailed())
	assert.True(t, m.rs.done)
	assert.False(t, m.rs.success)
	assert.Equal(t, nDeploy, m.graph.selected, "a failure takes the selection")
	assert.Equal(t, phaseFailed, runPhase(m.rs, m.cancelLevel))
}

func TestMovingTheSelectionStopsAutoSelect(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, ev(started(), running(nGuard, 0)))
	assert.Equal(t, nGuard, m.graph.selected)

	m = send(t, m, press("l"))
	assert.Equal(t, nUICheck, m.graph.selected)
	assert.Equal(t, nUICheck, m.pane.node)
	assert.False(t, m.auto)

	m = send(t, m, ev(ended(nGuard, events.Succeeded, "", 0, 1), running(nRelease, 1)))
	assert.Equal(t, nUICheck, m.graph.selected, "the user's choice sticks")
}

func TestLayoutToggleKeepsAutoSelect(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, press("v"))
	assert.True(t, m.auto)
}

func TestFailedRunEndsOnTheFirstFailure(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, ev(
		started(),
		running(nGuard, 0), ended(nGuard, events.Succeeded, "", 0, 1),
		running(nGoCheck, 1), running(nUICheck, 1),
		ended(nGoCheck, events.Failed, "exit code 2", 1, 2),
		ended(nUICheck, events.Failed, "exit code 3", 1, 3),
	))
	assert.Equal(t, nGoCheck, m.graph.selected)
	m.graph = m.graph.selectNode(nGuard)
	m = send(t, m, ev(finished(false, 3)))
	assert.Equal(t, nGoCheck, m.graph.selected, "at the end an untouched selection shows the first failure")
}

func TestOutputIsSanitizedAndDropsAreNoted(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, ev(
		running(nGuard, 0),
		output(nGuard, "\x1b[2Jclear screen\r\nok"),
		events.OutputDropped{Node: nGuard, Lines: 42},
		output("nope", "unknown node is ignored"),
		events.OutputDropped{Node: "nope", Lines: 1},
	))
	buf := m.rs.logs[nGuard]
	require.Equal(t, 2, buf.end())
	assert.Equal(t, "ok", buf.at(0).Text)
	assert.True(t, buf.at(1).Notice)
	assert.Contains(t, buf.at(1).Text, "42 lines not shown here")
	assert.Contains(t, buf.at(1).Text, testRun().Dir+"/nodes/guard/1.log")
}

func TestBarrierIsAcknowledged(t *testing.T) {
	bus := events.NewBus()
	sub := bus.Subscribe(0)
	defer bus.Close()
	reached := make(chan bool, 1)
	go func() { reached <- sub.Barrier(5 * time.Second) }()
	b, ok := (<-sub.C).(events.Barrier)
	require.True(t, ok)

	m, _ := newModel(t, 160, 45)
	_, cmd := update(t, m, ev(b))
	runCmd(cmd)
	assert.True(t, <-reached)
}

func TestCtrlCCancelsThenKillsThenQuits(t *testing.T) {
	m, fi := newModel(t, 160, 45)
	m = send(t, m, checksPassed())

	m, cmd := update(t, m, press("ctrl+c"))
	msgs := runCmd(cmd)
	require.Equal(t, []tea.Msg{interruptedMsg{level: 1, signal: ""}}, msgs)
	assert.Equal(t, 1, fi.calls)
	m = send(t, m, msgs...)
	assert.Equal(t, phaseCanceling, runPhase(m.rs, m.cancelLevel))
	assert.Contains(t, m.footer.notice, "ctrl+c again kills them now")

	m, cmd = update(t, m, press("ctrl+c"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, 2, fi.calls)
	assert.Equal(t, phaseKilling, runPhase(m.rs, m.cancelLevel))

	m = send(t, m, ev(notRun(nRelease, events.Canceled, "canceled (killed)", 4), finished(false, 4)))
	assert.Equal(t, phaseCanceled, runPhase(m.rs, m.cancelLevel))
	_, cmd = update(t, m, press("ctrl+c"))
	assert.True(t, hasQuit(runCmd(cmd)), "after the run ends ctrl+c quits")
	assert.Equal(t, 2, fi.calls)
}

func TestSignalInterruptShowsInFooter(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), interruptedMsg{level: 1, signal: "terminated"})
	assert.Equal(t, phaseCanceling, runPhase(m.rs, m.cancelLevel))
	assert.Contains(t, m.footer.notice, "canceling (terminated)")
}

func TestQuitDuringRunAsksFirst(t *testing.T) {
	m, fi := newModel(t, 160, 45)
	m = send(t, m, checksPassed())

	m, cmd := update(t, m, press("q"))
	assert.Nil(t, cmd)
	assert.Equal(t, askQuit, m.ask.kind)
	require.NotNil(t, m.dialog)
	assert.Equal(t, "Quit while the run is going?", m.dialog.title)
	assert.Contains(t, screen(m), "Cancel run and quit")

	m, cmd = update(t, m, press("j"))
	assert.Nil(t, cmd)
	assert.NotNil(t, m.dialog, "a key the dialog does not use keeps it open")
	assert.Equal(t, nRelease, m.graph.selected, "and does not reach the graph")

	m, cmd = update(t, m, press("enter"))
	assert.Nil(t, cmd, "enter picks the selected button, which starts on No")
	assert.Nil(t, m.dialog)
	assert.Equal(t, askNone, m.ask.kind)
	assert.Empty(t, m.footer.notice)

	m = send(t, m, press("q"))
	m, cmd = update(t, m, press("y"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, 1, fi.calls)
	assert.True(t, m.quitAfterRun)
	assert.Contains(t, m.footer.notice, "weft quits when the nodes stop")

	_, cmd = update(t, m, ev(notRun(nRelease, events.Canceled, "canceled", 4), finished(false, 4)))
	assert.True(t, hasQuit(runCmd(cmd)))
}

func TestQuitAfterRunEnds(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed())
	m, cmd := update(t, m, deployFailed())
	assert.False(t, hasQuit(runCmd(cmd)), "the TUI stays open after the run")
	_, cmd = update(t, m, press("q"))
	assert.True(t, hasQuit(runCmd(cmd)))
}

func TestExitOnEnd(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m.opts.ExitOnEnd = true
	m = send(t, m, checksPassed())
	_, cmd := update(t, m, deployFailed())
	assert.True(t, hasQuit(runCmd(cmd)))
}

func TestTickStopsWhenTheRunEnds(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	_, cmd := update(t, m, tickMsg{at: at(1)})
	assert.NotNil(t, cmd)
	m = send(t, m, checksPassed(), deployFailed())
	_, cmd = update(t, m, tickMsg{at: at(5)})
	assert.Nil(t, cmd)
}

func TestFullScreenLog(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	m = send(t, m, checksPassed(), press("enter"))
	assert.True(t, m.viewOpen)
	assert.Equal(t, nRelease, m.view.node)
	assert.Equal(t, 24-2-1, m.view.height)

	m = send(t, m, press("k"))
	assert.Equal(t, nRelease, m.graph.selected, "keys scroll the log, not the graph")

	m = send(t, m, ev(running(nDeploy, 3)))
	assert.Equal(t, nRelease, m.view.node, "the open log stays on its node")

	m = send(t, m, press("esc"))
	assert.False(t, m.viewOpen)
	m = send(t, m, press("enter"), press("q"))
	assert.False(t, m.viewOpen, "q closes the log view")
	assert.Equal(t, askNone, m.ask.kind, "and does not ask to quit")
}

func TestHelpDialog(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, press("?"))
	require.NotNil(t, m.dialog)
	out := screen(m)
	assert.Contains(t, out, "Keyboard shortcuts")
	assert.Contains(t, out, "rerun it and everything downstream")
	assert.Contains(t, out, "release", "the graph shows around the dialog")

	before := m.graph.selected
	m = send(t, m, press("j"))
	assert.Equal(t, before, m.graph.selected, "keys scroll the help, not the graph")
	m = send(t, m, press("?"))
	assert.Nil(t, m.dialog, "? closes it again")

	m = send(t, m, press("enter"), press("?"))
	require.NotNil(t, m.dialog)
	assert.Contains(t, screen(m), "back to the graph", "the log view has its own help")
	m = send(t, m, press("esc"))
	assert.Nil(t, m.dialog)
	assert.True(t, m.viewOpen, "esc closes the help, not the log")
}

func TestHelpDialogScrolls(t *testing.T) {
	m, _ := newModel(t, 60, 14)
	m = send(t, m, press("?"))
	require.NotNil(t, m.dialog)
	assert.Contains(t, screen(m), "Keyboard shortcuts (")
	assert.NotContains(t, screen(m), "show or hide this help", "the last row is below the fold")
	m = send(t, m, press("G"))
	assert.Contains(t, screen(m), "show or hide this help")
	assert.Contains(t, screen(m), "(100%)")
}

func TestNodeInfoDialog(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), deployFailed(), press("i"))
	require.NotNil(t, m.dialog)
	out := screen(m)
	assert.Equal(t, nDeploy, m.dialog.title, "the failed node, which the end selects")
	for _, want := range []string{"needs             release", "needed by         tofu-apply", "kill_timeout      30s", "exit code         1"} {
		assert.Contains(t, out, want)
	}
	m = send(t, m, press("i"))
	assert.Nil(t, m.dialog)
}

func TestCtrlCThroughADialog(t *testing.T) {
	m, fi := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), press("?"))
	m, cmd := update(t, m, press("ctrl+c"))
	assert.Nil(t, m.dialog, "ctrl+c closes the help")
	runCmd(cmd)
	assert.Equal(t, 1, fi.calls, "and still cancels the run")
}

// TestViewFillsTheScreenExactly is the guard against screen corruption: at
// every size and in every state, View has exactly one row per terminal row
// and no row is wider than the terminal.
func TestViewFillsTheScreenExactly(t *testing.T) {
	sizes := [][2]int{{160, 45}, {80, 24}, {40, 12}, {30, 10}, {29, 10}, {80, 9}, {200, 60}, {12, 3}}
	setups := map[string][]tea.Msg{
		"start":     nil,
		"running":   {checksPassed()},
		"failed":    {checksPassed(), deployFailed()},
		"full log":  {checksPassed(), press("enter")},
		"help":      {checksPassed(), press("?")},
		"confirm":   {checksPassed(), press("q")},
		"canceling": {checksPassed(), interruptedMsg{level: 1, signal: ""}},
		"vertical":  {checksPassed(), press("v")},
	}
	for name, msgs := range setups {
		for _, sz := range sizes {
			m, _ := newModel(t, sz[0], sz[1])
			m = send(t, m, msgs...)
			lines := splitLines(m.View().Content)
			if sz[0] < minWidth || sz[1] < minHeight {
				assert.Len(t, lines, 1, "%s at %dx%d", name, sz[0], sz[1])
			} else {
				assert.Len(t, lines, sz[1], "%s at %dx%d", name, sz[0], sz[1])
			}
			for i, l := range lines {
				assert.LessOrEqual(t, ansi.StringWidth(l), sz[0], "%s at %dx%d, row %d: %q", name, sz[0], sz[1], i, ansi.Strip(l))
			}
		}
	}
}

func TestViewBeforeTheFirstSize(t *testing.T) {
	m := New(Options{Workflow: deployWorkflow(t), Run: testRun(), ExitOnEnd: false, Interrupt: nil, Log: zerolog.Nop()})
	assert.Empty(t, m.View().Content)
	assert.True(t, m.View().AltScreen)
}

func TestEmptyWorkflow(t *testing.T) {
	// Validation rejects a workflow with no nodes; the TUI copes anyway.
	wf := &workflow.Workflow{Name: "empty", Path: "", Root: "", Hash: "", Concurrency: 1, Nodes: nil}
	m := New(Options{Workflow: wf, Run: testRun(), ExitOnEnd: false, Interrupt: nil, Log: zerolog.Nop()})
	m = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 24}, ev(started(), finished(true, 0)), press("j"), press("enter"))
	out := m.View().Content
	assert.Len(t, splitLines(out), 24)
	assert.Contains(t, ansi.Strip(out), "this workflow has no nodes")
	assert.Contains(t, ansi.Strip(out), "succeeded")
}

func screen(m Model) string {
	lines := splitLines(ansi.Strip(m.View().Content))
	for i, l := range lines {
		lines[i] = trimRight(l)
	}
	return joinLines(lines)
}

func TestGoldenRunning(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), tickMsg{at: at(93.2)})
	golden.RequireEqual(t, screen(m))
}

func TestGoldenFailedWithBlockedDownstream(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), deployFailed())
	golden.RequireEqual(t, screen(m))
}

func TestGoldenFinished(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), ev(
		ended(nRelease, events.Succeeded, "", 2.2, 3),
		running(nDeploy, 3), ended(nDeploy, events.Succeeded, "", 3, 3.5),
		running(nTofu, 3.5), ended(nTofu, events.Succeeded, "", 3.5, 9),
		finished(true, 9),
	))
	golden.RequireEqual(t, screen(m))
}

func TestGoldenNarrowFallsBackToVertical(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	m = send(t, m, checksPassed(), tickMsg{at: at(10)})
	golden.RequireEqual(t, screen(m))
}

func TestGoldenFullScreenLog(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	m = send(t, m, checksPassed(), deployFailed(), press("enter"))
	golden.RequireEqual(t, screen(m))
}

func TestHeaderKeepsCountsAndDirLabelWhenNarrow(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	id := testRun().ID
	m.rs.run = &store.Run{ID: id, Dir: "/Users/someone/src/github.com/example/project/.weft/runs/deploy/" + id, Workflow: workflowName}
	m = send(t, m, checksPassed(), deployFailed())
	rows := splitLines(ansi.Strip(m.View().Content))
	assert.Contains(t, rows[0], "5 succeeded, 1 failed, 1 blocked")
	assert.NotContains(t, rows[0], "run 2026", "the run ID gives way first")
	assert.True(t, strings.HasPrefix(rows[1], " run dir …"), rows[1])
	assert.True(t, strings.HasSuffix(rows[1], testRun().ID), rows[1])
}

func TestGoldenHelpDialog(t *testing.T) {
	m, _ := newModel(t, 100, 40)
	m = send(t, m, checksPassed(), press("?"))
	golden.RequireEqual(t, screen(m))
}

func TestGoldenQuitDialog(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	m = send(t, m, checksPassed(), press("q"))
	golden.RequireEqual(t, screen(m))
}

func TestGoldenNodeInfoDialog(t *testing.T) {
	m, _ := newModel(t, 120, 45)
	m = send(t, m, checksPassed(), deployFailed(), press("i"))
	golden.RequireEqual(t, screen(m))
}

func TestHangingWrap(t *testing.T) {
	assert.Equal(t, []string{"log  /a/b"}, hangingWrap("log  /a/b", 20, 5))
	assert.Equal(t, []string{"dir  one two", "     three four"}, hangingWrap("dir  one two three four", 15, 5))
	assert.Equal(t, []string{"run", "     aaaaaaa", "     bbbbbbb", "     ccc"}, hangingWrap("run  aaaaaaabbbbbbbccc", 12, 5),
		"a word too long for the first row starts on the next and breaks there")
}
