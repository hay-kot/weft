package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"

	"github.com/hay-kot/weft/internal/events"
)

func fill(rs *runState, id string, n int) {
	for i := range n {
		rs.logs[id].append(logLine{Text: fmt.Sprintf("line %d", i), Notice: false})
	}
}

func TestLogPaneShowResetsScrollForANewNode(t *testing.T) {
	p := newLogPane(nGuard)
	p.scroll = scroller{top: 3, follow: false}
	assert.Equal(t, p.scroll, p.show(nGuard).scroll, "same node keeps its position")
	assert.Equal(t, newScroller(), p.show(nRelease).scroll)
}

func TestLogPaneKeys(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 100)
	fill(rs, nGuard, 50)
	keys := DefaultKeyMap()
	buf := rs.logs[nGuard]
	tests := []struct {
		name   string
		start  scroller
		key    string
		want   scroller
		handle bool
	}{
		{"page up holds half a page above the tail", newScroller(), "pgup", scroller{top: 35, follow: false}, true},
		{"ctrl+u is page up", newScroller(), "ctrl+u", scroller{top: 35, follow: false}, true},
		{"page down at the bottom follows", scroller{top: 35, follow: false}, "pgdown", scroller{top: 40, follow: true}, true},
		{"g goes to the top", newScroller(), "g", scroller{top: 0, follow: false}, true},
		{"G follows", scroller{top: 0, follow: false}, "G", scroller{top: 0, follow: true}, true},
		{"f pauses", newScroller(), "f", scroller{top: 40, follow: false}, true},
		{"other keys pass through", newScroller(), "x", newScroller(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newLogPane(nGuard).withSize(80, 10)
			p.scroll = tt.start
			msg := press(tt.key)
			if tt.key == "ctrl+u" {
				msg.Code, msg.Text, msg.Mod = 'u', "", tea.ModCtrl
			}
			got, handled := p.Update(msg, keys, buf)
			assert.Equal(t, tt.handle, handled)
			assert.Equal(t, tt.want, got.scroll)
		})
	}
}

func TestLogPaneViewIsExactlyItsSize(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 100)
	rs.logs[nGuard].append(logLine{Text: strings.Repeat("wide ", 100), Notice: false})
	rs.logs[nGuard].append(logLine{Text: "\x1b[31munterminated red", Notice: false})
	for _, w := range []int{20, 80, 160} {
		for _, h := range []int{0, 1, 5} {
			p := newLogPane(nGuard).withSize(w, h)
			lines := splitLines(p.View(rs, DefaultStyles(), t0))
			assert.Len(t, lines, h+1, "%dx%d", w, h)
			for _, l := range lines {
				assert.LessOrEqual(t, ansi.StringWidth(l), w, "%dx%d: %q", w, h, l)
			}
		}
	}
}

func TestRenderLogLine(t *testing.T) {
	s := DefaultStyles()
	assert.Equal(t, " hello", renderLogLine(logLine{Text: "hello", Notice: false}, 0, 80, s))
	assert.Equal(t, " hel…", renderLogLine(logLine{Text: "hello world", Notice: false}, 0, 5, s))
	assert.Equal(t, " world", renderLogLine(logLine{Text: "hello world", Notice: false}, 6, 80, s))
	assert.Equal(t, " \x1b[31mred"+ansi.ResetStyle, renderLogLine(logLine{Text: "\x1b[31mred", Notice: false}, 0, 80, s),
		"a color left on is reset at the end of the row")
	assert.Empty(t, renderLogLine(logLine{Text: "x", Notice: false}, 0, 1, s))
	assert.Equal(t, " note", ansi.Strip(renderLogLine(logLine{Text: "note", Notice: true}, 0, 80, s)))
}

func TestStatusText(t *testing.T) {
	tests := []struct {
		st   *nodeStatus
		want string
	}{
		{status(events.Failed, "exit code 1"), "failed: exit code 1"},
		{status(events.Failed, ""), "failed"},
		{status(events.Blocked, "blocked: deploy failed"), "blocked: deploy failed"},
		{status(events.Canceled, ""), "canceled"},
		{status(events.Waiting, ""), "waiting for the terminal"},
		{status(events.Running, ""), "running"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, statusText(tt.st))
	}
}

func TestPlaceholder(t *testing.T) {
	tests := []struct {
		name  string
		setup func(rs *runState)
		node  string
		want  string
	}{
		{"pending lists unmet needs", func(rs *runState) { rs.nodes[nGoCheck].State = events.Succeeded }, nRelease, "waiting on ui-check, db-backup"},
		{"ready waits for a slot", func(rs *runState) { rs.nodes[nGuard].State = events.Ready }, nGuard, "waiting for a free slot"},
		{"waiting for the terminal", func(rs *runState) { rs.nodes[nTofu].State = events.Waiting }, nTofu, "waiting for running nodes to finish, then it takes the terminal"},
		{"running without output", func(rs *runState) { rs.nodes[nGuard].State = events.Running }, nGuard, "no output yet"},
		{"interactive running", func(rs *runState) { rs.nodes[nTofu].State = events.Running }, nTofu, "has the terminal; its output goes there, not to this log"},
		{"interactive finished", func(rs *runState) { rs.nodes[nTofu].State = events.Succeeded }, nTofu, "interactive: its output went to the terminal and was not captured"},
		{"blocked never ran", func(rs *runState) { rs.nodes[nDeploy].State = events.Blocked }, nDeploy, "did not run"},
		{"finished without output", func(rs *runState) { rs.nodes[nGuard].State = events.Succeeded }, nGuard, "no output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRunState(deployWorkflow(t), testRun(), nil, 10)
			tt.setup(rs)
			assert.Equal(t, tt.want, placeholder(rs, tt.node))
		})
	}
}

func TestLogViewSidewaysScrollStopsAtTheWidestLine(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 100)
	rs.logs[nGuard].append(logLine{Text: strings.Repeat("x", 30), Notice: false})
	v := newLogView(nGuard).withSize(20, 5)
	keys := DefaultKeyMap()
	for range 5 {
		v, _ = v.Update(press("l"), keys, rs.logs[nGuard])
	}
	assert.Equal(t, 10, v.left)
	v, _ = v.Update(press("h"), keys, rs.logs[nGuard])
	assert.Equal(t, 2, v.left)
	v, _ = v.Update(press("h"), keys, rs.logs[nGuard])
	assert.Equal(t, 0, v.left)
}

func TestLogViewLineScroll(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 100)
	fill(rs, nGuard, 30)
	v := newLogView(nGuard).withSize(80, 10)
	keys := DefaultKeyMap()
	v, _ = v.Update(press("k"), keys, rs.logs[nGuard])
	assert.Equal(t, scroller{top: 19, follow: false}, v.scroll)
	v, _ = v.Update(press("j"), keys, rs.logs[nGuard])
	assert.True(t, v.scroll.follow)
	_, handled := v.Update(press("q"), keys, rs.logs[nGuard])
	assert.False(t, handled, "closing is the model's job")
}

func TestFit(t *testing.T) {
	assert.Equal(t, "left     right", fit("left", "right", 14))
	assert.Equal(t, "a long left s… right", fit("a long left side", "right", 20))
	assert.Equal(t, "a long…", fit("a long left side", "right", 7))
	assert.Equal(t, "left", fit("left", "", 14))
}

func TestLogPaneGolden(t *testing.T) {
	rs := newRunState(deployWorkflow(t), testRun(), nil, 100)
	st := rs.nodes[nRelease]
	st.State, st.Started = events.Running, t0
	for _, l := range []string{"\x1b[36m•\x1b[0m building binaries  layout=dist", "• publishing  example.invalid/stub:v1.42.0"} {
		rs.logs[nRelease].append(logLine{Text: sanitize(l), Notice: false})
	}
	rs.logs[nRelease].append(logLine{Text: "… 12 lines not shown", Notice: true})
	p := newLogPane(nRelease).withSize(80, 5)
	golden.RequireEqual(t, ansi.Strip(p.View(rs, DefaultStyles(), t0.Add(91*time.Second))))
}
