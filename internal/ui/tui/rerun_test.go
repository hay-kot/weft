package tui

import (
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/store"
)

type rerunCall struct {
	node       string
	downstream bool
	force      bool
}

// fakeRerun records rerun requests and answers them with errFor.
type fakeRerun struct {
	calls  []rerunCall
	errFor func(rerunCall) error
}

func (f *fakeRerun) Rerun(id string, downstream, force bool) error {
	c := rerunCall{node: id, downstream: downstream, force: force}
	f.calls = append(f.calls, c)
	if f.errFor != nil {
		return f.errFor(c)
	}
	return nil
}

func newRerunModel(t *testing.T, fr *fakeRerun) Model {
	t.Helper()
	m, _ := newModel(t, 160, 45)
	m.opts.Rerun = fr.Rerun
	return m
}

// selectDeploy checks that the selection is on deploy, where a failed run
// leaves it.
func selectDeploy(t *testing.T, m Model) Model {
	t.Helper()
	require.Equal(t, nDeploy, m.graph.selected)
	return m
}

func runningAttempt(node string, attempt int, sec float64) events.NodeChanged {
	e := running(node, sec)
	e.Attempt = attempt
	return e
}

func pass(n int, sec float64) events.RunStarted {
	e := started()
	e.Pass, e.At = n, at(sec)
	return e
}

func TestRerunAfterTheRunEnds(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: nil}
	m := newRerunModel(t, fr)
	m = send(t, m, checksPassed(), deployFailed(), tickMsg{at: at(4)})
	require.True(t, m.rs.done)
	assert.False(t, m.ticking, "the clock stopped with the run")
	m = selectDeploy(t, m)

	m, cmd := update(t, m, press("r"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, []rerunCall{{node: nDeploy, downstream: false, force: false}}, fr.calls)
	assert.Equal(t, "rerunning deploy", m.footer.notice)

	m, cmd = update(t, m, ev(pass(2, 10), events.NodeChanged{
		Node: nDeploy, State: events.Pending, Attempt: 2, ExitCode: 0, Err: "", Reason: "",
		Started: time.Time{}, Finished: time.Time{}, At: at(10),
	}, runningAttempt(nDeploy, 2, 10)))
	assert.False(t, m.rs.done, "the run is active again")
	assert.True(t, m.ticking)
	require.NotNil(t, cmd, "the clock restarts")
	assert.Equal(t, phaseRunning, runPhase(m.rs, m.cancelLevel))
	short, _ := m.footerHelp()
	assert.True(t, short[2].Enabled(), "ctrl+c cancel shows again")
	assert.Equal(t, 2, m.rs.nodes[nDeploy].Attempt)

	lines := m.rs.logs[nDeploy].slice(m.rs.logs[nDeploy].first(), m.rs.logs[nDeploy].end())
	last := lines[len(lines)-1]
	assert.Equal(t, logLine{Text: "── attempt 2 ──", Notice: true}, last, "earlier output stays, marked off")

	m = send(t, m, ev(ended(nDeploy, events.Succeeded, "", 10, 11), finished(false, 11)))
	assert.True(t, m.rs.done)
	assert.Equal(t, phaseFailed, runPhase(m.rs, m.cancelLevel), "tofu-apply is still blocked")
}

func TestRerunWithDownstream(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: nil}
	m := newRerunModel(t, fr)
	m = send(t, m, checksPassed(), deployFailed())
	m = selectDeploy(t, m)
	m, cmd := update(t, m, press("R"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, []rerunCall{{node: nDeploy, downstream: true, force: false}}, fr.calls)
	assert.Equal(t, "rerunning deploy and everything downstream of it", m.footer.notice)
}

func TestRerunRefusedLocally(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: nil}
	m := newRerunModel(t, fr)
	m = send(t, m, checksPassed(), deployFailed())
	m = send(t, m, press("l"))
	require.Equal(t, nTofu, m.graph.selected)

	m, cmd := update(t, m, press("r"))
	assert.Nil(t, cmd)
	assert.Empty(t, fr.calls)
	assert.Equal(t, "can't rerun tofu-apply: it needs deploy, which is failed", m.footer.notice)
	assert.Equal(t, noticeWarn, m.footer.noticeKind)
}

func TestRerunOfARunningNodeIsRefused(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: nil}
	m := newRerunModel(t, fr)
	m = send(t, m, checksPassed())
	require.Equal(t, nRelease, m.graph.selected)
	m, cmd := update(t, m, press("r"))
	assert.Nil(t, cmd)
	assert.Equal(t, "can't rerun release: it is running", m.footer.notice)
}

func TestRerunWhileCanceling(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: nil}
	m := newRerunModel(t, fr)
	m = send(t, m, checksPassed(), ev(ended(nRelease, events.Failed, "exit code 1", 2.2, 3)), interruptedMsg{level: 1, signal: ""})
	m, cmd := update(t, m, press("r"))
	assert.Nil(t, cmd)
	assert.Contains(t, m.footer.notice, "being canceled")
}

func TestRerunEngineErrorShows(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: func(rerunCall) error { return errors.New("the run is over and takes no more reruns") }}
	m := newRerunModel(t, fr)
	m = selectDeploy(t, send(t, m, checksPassed(), deployFailed()))
	m, cmd := update(t, m, press("r"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, "the run is over and takes no more reruns", m.footer.notice)
}

func TestRerunAsksWhenHeadMoved(t *testing.T) {
	fr := &fakeRerun{calls: nil, errFor: func(c rerunCall) error {
		if c.force {
			return nil
		}
		return &plan.DriftError{RunID: testRun().ID, Reasons: []string{"git HEAD moved from aaa to bbb"}}
	}}
	m := newRerunModel(t, fr)
	m = selectDeploy(t, send(t, m, checksPassed(), deployFailed()))

	m, cmd := update(t, m, press("R"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, askRerun, m.ask.kind)
	assert.Equal(t, "Since this run last started, git HEAD moved from aaa to bbb. y reruns deploy anyway, any other key skips it.", m.footer.notice)

	t.Run("y reruns with force", func(t *testing.T) {
		m, cmd := update(t, m, press("y"))
		m = send(t, m, runCmd(cmd)...)
		assert.Equal(t, askNone, m.ask.kind)
		assert.Equal(t, rerunCall{node: nDeploy, downstream: true, force: true}, fr.calls[len(fr.calls)-1])
		assert.Equal(t, "rerunning deploy and everything downstream of it", m.footer.notice)
	})
	t.Run("any other key skips it", func(t *testing.T) {
		before := len(fr.calls)
		m, cmd := update(t, m, press("n"))
		assert.Nil(t, cmd)
		assert.Len(t, fr.calls, before)
		assert.Equal(t, "rerun of deploy skipped", m.footer.notice)
		assert.Equal(t, nDeploy, m.graph.selected)
	})
	t.Run("the question outlasts the end of a pass", func(t *testing.T) {
		m := send(t, m, ev(finished(false, 20)))
		assert.Equal(t, askRerun, m.ask.kind)
		assert.Contains(t, m.footer.notice, "y reruns deploy anyway")
	})
}

func newConfirmModel(t *testing.T, started *int) Model {
	t.Helper()
	m := New(Options{
		Workflow:  deployWorkflow(t),
		Run:       testRun(),
		ExitOnEnd: false,
		Interrupt: func() int { return 1 },
		Plan:      nil,
		Confirm:   "Since run x last started, git HEAD moved from a to b. Go ahead anyway? y goes ahead, any other key quits.",
		Start:     func() { *started++ },
		Rerun:     nil,
		Notice:    "--resume continues run x",
		Log:       zerolog.Nop(),
	})
	return send(t, m, tickMsg{at: t0}, tea.WindowSizeMsg{Width: 160, Height: 45})
}

func TestConfirmBeforeStart(t *testing.T) {
	t.Run("y starts", func(t *testing.T) {
		n := 0
		m := newConfirmModel(t, &n)
		assert.Equal(t, askStart, m.ask.kind)
		assert.Contains(t, m.footer.notice, "Go ahead anyway?")
		m, cmd := update(t, m, press("y"))
		runCmd(cmd)
		assert.Equal(t, 1, n)
		assert.Equal(t, "--resume continues run x", m.footer.notice)
	})
	t.Run("any other key quits without starting", func(t *testing.T) {
		n := 0
		m := newConfirmModel(t, &n)
		_, cmd := update(t, m, press("n"))
		assert.True(t, hasQuit(runCmd(cmd)))
		assert.Zero(t, n)
	})
	t.Run("ctrl+c quits without starting", func(t *testing.T) {
		n := 0
		m := newConfirmModel(t, &n)
		_, cmd := update(t, m, press("ctrl+c"))
		assert.True(t, hasQuit(runCmd(cmd)))
		assert.Zero(t, n)
	})
	t.Run("a signal quits without starting", func(t *testing.T) {
		n := 0
		m := newConfirmModel(t, &n)
		_, cmd := update(t, m, interruptedMsg{level: 1, signal: "terminated"})
		assert.True(t, hasQuit(runCmd(cmd)))
		assert.Zero(t, n)
	})
}

func TestPassFinishAndStartInOneBatchKeepsOneClock(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed())
	require.True(t, m.ticking)
	_, cmd := update(t, m, ev(finished(false, 3), pass(2, 3), runningAttempt(nRelease, 2, 3)))
	assert.Empty(t, runCmd(cmd), "the clock still runs, so no second tick starts")
}

func TestQuitQuestionEndsWithTheRun(t *testing.T) {
	m, _ := newModel(t, 160, 45)
	m = send(t, m, checksPassed(), press("q"))
	require.Equal(t, askQuit, m.ask.kind)
	m = send(t, m, deployFailed())
	assert.Equal(t, askNone, m.ask.kind)
	_, cmd := update(t, m, press("q"))
	assert.True(t, hasQuit(runCmd(cmd)), "q quits at once after the run")
}

// resumedPlan is the plan of resuming the run where deploy failed.
func resumedPlan() *plan.Plan {
	p := plan.Plan{Mode: store.ModeResume, Selection: nil, Continue: true, Nodes: map[string]plan.Node{}}
	done := func(from, to float64) plan.Node {
		return plan.Node{State: events.Skipped, Attempt: 1, Prior: store.NodeState{
			State: events.Succeeded, Attempt: 1, ExitCode: 0, Reason: "exit", Error: "",
			Started: at(from), Finished: at(to), Log: "", PGID: 0,
		}}
	}
	p.Nodes[nGuard] = done(0, 0.2)
	p.Nodes[nGoCheck] = done(0.2, 2.2)
	p.Nodes[nUICheck] = done(0.2, 1.7)
	p.Nodes[nDBBackup] = done(0.2, 1.2)
	p.Nodes[nRelease] = done(2.2, 3)
	empty := store.NodeState{State: "", Attempt: 0, ExitCode: 0, Reason: "", Error: "", Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0}
	p.Nodes[nDeploy] = plan.Node{State: events.Pending, Attempt: 2, Prior: empty}
	p.Nodes[nTofu] = plan.Node{State: events.Pending, Attempt: 2, Prior: empty}
	return &p
}

func TestPlanSeedsStatesBeforeEvents(t *testing.T) {
	m := New(Options{
		Workflow: deployWorkflow(t), Run: testRun(), ExitOnEnd: false, Interrupt: nil,
		Plan: resumedPlan(), Confirm: "", Start: nil, Rerun: nil, Notice: "", Log: zerolog.Nop(),
	})
	assert.Equal(t, events.Skipped, m.rs.state(nRelease))
	assert.Equal(t, events.Pending, m.rs.state(nDeploy))
	assert.Equal(t, 2, m.rs.nodes[nDeploy].Attempt)
	assert.Equal(t, "skipped: it succeeded in attempt 1, whose output is in /repo/.weft/runs/deploy/20261025T191200.000-a3f9/nodes/release/1.log",
		placeholder(m.rs, nRelease))
}

func TestGoldenResumed(t *testing.T) {
	m := New(Options{
		Workflow: deployWorkflow(t), Run: testRun(), ExitOnEnd: false, Interrupt: nil,
		Plan: resumedPlan(), Confirm: "", Start: nil, Rerun: nil,
		Notice: "--resume continues run 20261025T191200.000-a3f9: running deploy, tofu-apply; skipping 5 that succeeded",
		Log:    zerolog.Nop(),
	})
	m = send(t, m, tickMsg{at: at(30)}, tea.WindowSizeMsg{Width: 120, Height: 30},
		ev(pass(1, 30), runningAttempt(nDeploy, 2, 30), output(nDeploy, "deploy: rolling out v0.0.0-stub (stub)")),
		tickMsg{at: at(31)})
	golden.RequireEqual(t, screen(m))
}
