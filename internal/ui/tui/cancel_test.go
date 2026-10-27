package tui

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
)

// fakeCancelNode records node cancel requests and answers with replies in
// order.
type fakeCancelNode struct {
	calls   []string
	replies []plan.CancelAction
	err     error
}

func (f *fakeCancelNode) CancelNode(id string) (plan.CancelAction, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return 0, f.err
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func newCancelNodeModel(t *testing.T, fc *fakeCancelNode) Model {
	t.Helper()
	m, _ := newModel(t, 160, 45)
	m.opts.CancelNode = fc.CancelNode
	return send(t, m, checksPassed())
}

func TestCancelNodeRunningThenKill(t *testing.T) {
	fc := &fakeCancelNode{calls: nil, replies: []plan.CancelAction{plan.CancelRunning, plan.KillRunning}, err: nil}
	m := newCancelNodeModel(t, fc)
	require.Equal(t, nRelease, m.graph.selected, "the selection follows the run")

	m, cmd := update(t, m, press("x"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, []string{nRelease}, fc.calls)
	assert.Equal(t, noticeWarn, m.footer.noticeKind)
	assert.Equal(t, "canceling release: it got SIGINT and has its kill_timeout (30s) to stop. x again kills it now", m.footer.notice)
	assert.Equal(t, 0, m.cancelLevel, "the run itself is not canceling")

	m, cmd = update(t, m, press("x"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, []string{nRelease, nRelease}, fc.calls)
	assert.Equal(t, "killing release", m.footer.notice)

	e := ended(nRelease, events.Canceled, "canceled (killed)", 2.2, 5)
	m = send(t, m, ev(e))
	assert.Equal(t, noticeInfo, m.footer.noticeKind)
	assert.Equal(t, "release canceled (killed)", m.footer.notice, "the notice says how the node ended")
}

func TestCancelNoticeKeepsANewerNotice(t *testing.T) {
	fc := &fakeCancelNode{calls: nil, replies: []plan.CancelAction{plan.CancelRunning}, err: nil}
	m := newCancelNodeModel(t, fc)
	m, cmd := update(t, m, press("x"))
	m = send(t, m, runCmd(cmd)...)
	m = send(t, m, press("h"), press("h"), press("x"))
	require.Equal(t, "can't cancel guard: it is succeeded", m.footer.notice)

	m = send(t, m, ev(ended(nRelease, events.Canceled, "canceled (interrupt)", 2.2, 5)))
	assert.Equal(t, "can't cancel guard: it is succeeded", m.footer.notice)
	assert.Empty(t, m.stopping)
}

func TestCancelNodeBeforeItStarts(t *testing.T) {
	fc := &fakeCancelNode{calls: nil, replies: []plan.CancelAction{plan.CancelUnstarted}, err: nil}
	m := newCancelNodeModel(t, fc)
	m = send(t, m, press("l"))
	require.Equal(t, nDeploy, m.graph.selected)

	m, cmd := update(t, m, press("x"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, []string{nDeploy}, fc.calls)
	assert.Equal(t, "canceled deploy before it started", m.footer.notice)
}

func TestCancelNodeRefusedLocally(t *testing.T) {
	fc := &fakeCancelNode{calls: nil, replies: nil, err: nil}
	m := newCancelNodeModel(t, fc)
	m = send(t, m, press("h"), press("h"))
	require.Equal(t, nGuard, m.graph.selected)

	m, cmd := update(t, m, press("x"))
	assert.Nil(t, cmd)
	assert.Empty(t, fc.calls)
	assert.Equal(t, noticeWarn, m.footer.noticeKind)
	assert.Equal(t, "can't cancel guard: it is succeeded", m.footer.notice)
}

func TestCancelNodeEngineError(t *testing.T) {
	fc := &fakeCancelNode{calls: nil, replies: nil, err: errors.New("can't cancel release: it is succeeded")}
	m := newCancelNodeModel(t, fc)
	m, cmd := update(t, m, press("x"))
	m = send(t, m, runCmd(cmd)...)
	assert.Equal(t, "can't cancel release: it is succeeded", m.footer.notice)
}

func TestCancelKeysInHelp(t *testing.T) {
	m := newCancelNodeModel(t, &fakeCancelNode{calls: nil, replies: nil, err: nil})
	enabled := func(m Model) map[string]bool {
		out := map[string]bool{}
		for _, b := range m.footerHelp() {
			out[b.Help().Key] = b.Enabled()
		}
		return out
	}
	assert.True(t, enabled(m)["x"])
	assert.True(t, enabled(m)["ctrl+c"])
	assert.Contains(t, helpKeys(m.keys.graphHelp()), "x")

	m = send(t, m, deployFailed())
	assert.False(t, enabled(m)["x"], "nothing to cancel after the run")
	assert.False(t, enabled(m)["ctrl+c"])

	ask, _ := newModel(t, 160, 45)
	ask.ask = question{kind: askStart, node: "", downstream: false}
	assert.False(t, enabled(ask)["ctrl+c"], "ctrl+c quits before the start, it cancels nothing")
	assert.False(t, enabled(ask)["x"])
}

func helpKeys(sections []helpSection) []string {
	var out []string
	for _, sec := range sections {
		for _, e := range sec.entries {
			out = append(out, e.binding.Help().Key)
		}
	}
	return out
}
