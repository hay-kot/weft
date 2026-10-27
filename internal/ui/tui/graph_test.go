package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
)

func press(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Text: "", Mod: 0, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape, Text: "", Mod: 0, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Text: "", Mod: tea.ModCtrl, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp, Text: "", Mod: 0, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown, Text: "", Mod: 0, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
	case "G":
		return tea.KeyPressMsg{Code: 'g', Text: "G", Mod: tea.ModShift, ShiftedCode: 'G', BaseCode: 0, IsRepeat: false}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s, Mod: 0, ShiftedCode: 0, BaseCode: 0, IsRepeat: false}
}

func deployPane(t *testing.T, width, height int) (graphPane, *runState) {
	t.Helper()
	wf := deployWorkflow(t)
	rs := newRunState(wf, testRun(), nil, 10)
	p := newGraphPane(layout.FromWorkflow(wf), rs.cells(t0))
	return p.withSize(width, height), rs
}

func TestKeyHelperMatchesBubbleTea(t *testing.T) {
	for _, s := range []string{"enter", "esc", "ctrl+c", "pgup", "pgdown", "G", "j", "?"} {
		assert.Equal(t, s, press(s).String())
	}
}

func TestGraphSelectionHorizontal(t *testing.T) {
	keys := DefaultKeyMap()
	tests := []struct {
		name  string
		from  string
		moves string
		want  string
	}{
		{"right from the root goes to the middle child", nGuard, "l", nUICheck},
		{"up and down step within a level", nUICheck, "k", nGoCheck},
		{"down from the middle", nUICheck, "j", nDBBackup},
		{"up stops at the top", nGoCheck, "k", nGoCheck},
		{"down stops at the bottom", nDBBackup, "j", nDBBackup},
		{"right converges on the fan-in", nDBBackup, "l", nRelease},
		{"left from the fan-in picks the nearest parent", nRelease, "h", nUICheck},
		{"left stops at the root", nGuard, "h", nGuard},
		{"right stops at the last level", nTofu, "l", nTofu},
		{"a path across the graph", nGuard, "lkllll", nTofu},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := deployPane(t, 200, 20)
			p = p.selectNode(tt.from)
			for _, r := range tt.moves {
				var handled bool
				p, handled = p.Update(press(string(r)), keys)
				assert.True(t, handled)
			}
			assert.Equal(t, tt.want, p.selected)
		})
	}
}

func TestGraphSelectionVertical(t *testing.T) {
	keys := DefaultKeyMap()
	tests := []struct {
		from, moves, want string
	}{
		{nGuard, "j", nUICheck},
		{nUICheck, "h", nGoCheck},
		{nUICheck, "l", nDBBackup},
		{nGoCheck, "j", nRelease},
		{nRelease, "k", nUICheck},
		{nDeploy, "l", nDeploy},
	}
	for _, tt := range tests {
		t.Run(tt.from+"/"+tt.moves, func(t *testing.T) {
			p, _ := deployPane(t, 40, 30)
			assert.Equal(t, layout.Vertical, p.grid.Orientation, "40 columns is too narrow for horizontal")
			p = p.selectNode(tt.from)
			for _, r := range tt.moves {
				p, _ = p.Update(press(string(r)), keys)
			}
			assert.Equal(t, tt.want, p.selected)
		})
	}
}

func TestGraphIgnoresOtherKeys(t *testing.T) {
	p, _ := deployPane(t, 200, 20)
	_, handled := p.Update(press("x"), DefaultKeyMap())
	assert.False(t, handled)
	_, handled = p.Update(tea.WindowSizeMsg{Width: 1, Height: 1}, DefaultKeyMap())
	assert.False(t, handled)
}

func TestGraphToggleLayoutSticks(t *testing.T) {
	p, _ := deployPane(t, 200, 30)
	assert.Equal(t, layout.Horizontal, p.grid.Orientation)
	p, _ = p.Update(press("v"), DefaultKeyMap())
	assert.Equal(t, layout.Vertical, p.grid.Orientation)
	p = p.withSize(220, 30)
	assert.Equal(t, layout.Vertical, p.grid.Orientation, "a resize keeps the chosen layout")
	p = p.toggleLayout()
	assert.Equal(t, layout.Horizontal, p.grid.Orientation)
}

func TestGraphScrollsSelectionIntoView(t *testing.T) {
	// Vertical layout of the fixture is 17 rows; show 5 of them.
	p, _ := deployPane(t, 40, 5)
	assert.Equal(t, 5, p.rows())
	assert.Equal(t, 17, p.fullHeight())
	assert.Equal(t, 0, p.offY)

	p = p.selectNode(nTofu)
	b := p.grid.Boxes[nTofu]
	assert.GreaterOrEqual(t, b.Y, p.offY)
	assert.Less(t, b.Y, p.offY+p.rows())

	p = p.selectNode(nGuard)
	assert.Equal(t, 0, p.offY)

	// Horizontal layout in a pane narrower than the graph.
	p = p.toggleLayout().withSize(30, 5)
	p = p.selectNode(nTofu)
	b = p.grid.Boxes[nTofu]
	assert.GreaterOrEqual(t, b.X, p.offX)
	assert.LessOrEqual(t, b.X+b.Width, p.offX+30)
}

func TestGraphSelectUnknownNodeKeepsSelection(t *testing.T) {
	p, _ := deployPane(t, 200, 20)
	p = p.selectNode("nope")
	assert.Equal(t, nGuard, p.selected)
}

func TestGraphClassify(t *testing.T) {
	p, rs := deployPane(t, 200, 20)
	rs.nodes[nRelease].State = events.Running
	rs.nodes[nRelease].Started = t0
	p = p.withCells(rs.cells(t0)).selectNode(nRelease)
	active := map[int]bool{}
	for _, e := range p.grid.EdgesOf(nRelease) {
		active[e] = true
	}

	b := p.grid.Boxes[nRelease]
	icon := p.classify(b.X, b.Y, p.grid.At(b.X, b.Y), rs, active)
	assert.Equal(t, glyphClass{kind: glyphIcon, state: events.Running, selected: false}, icon)
	label := p.classify(b.X+2, b.Y, p.grid.At(b.X+2, b.Y), rs, active)
	assert.Equal(t, glyphClass{kind: glyphLabel, state: events.Running, selected: true}, label)
	suffix := p.classify(b.X+b.Width-1, b.Y, p.grid.At(b.X+b.Width-1, b.Y), rs, active)
	assert.Equal(t, glyphClass{kind: glyphSuffix, state: events.Running, selected: false}, suffix)

	// The gutter between release and deploy carries release's edge.
	edge := p.classify(b.X+b.Width+1, b.Y, p.grid.At(b.X+b.Width+1, b.Y), rs, active)
	assert.Equal(t, glyphEdgeActive, edge.kind)

	// The gutter between guard and ui-check is not release's.
	g := p.grid.Boxes[nGuard]
	other := p.classify(g.X+g.Width+1, g.Y, p.grid.At(g.X+g.Width+1, g.Y), rs, active)
	assert.Equal(t, glyphEdge, other.kind)
}

// markedText returns, per line, the text drawn bold and underlined, and
// reports whether any cell is drawn in reverse video.
func markedText(view string) ([]string, bool) {
	ss := uv.NewStyledString(view)
	area := ss.Bounds()
	buf := uv.NewScreenBuffer(area.Dx(), area.Dy())
	ss.Draw(buf, area)
	var marked []string
	reverse := false
	for y := range area.Dy() {
		var line strings.Builder
		for x := range area.Dx() {
			c := buf.CellAt(x, y)
			if c == nil {
				continue
			}
			if c.Style.Attrs&uv.AttrReverse != 0 {
				reverse = true
			}
			if c.Style.Attrs&uv.AttrBold != 0 && c.Style.Underline != uv.UnderlineNone {
				line.WriteString(c.Content)
			}
		}
		if line.Len() > 0 {
			marked = append(marked, line.String())
		}
	}
	return marked, reverse
}

func TestGraphViewSelectionMarker(t *testing.T) {
	p, rs := deployPane(t, 200, 20)
	rs.nodes[nGuard].State = events.Succeeded
	rs.nodes[nGoCheck].State = events.Running
	rs.nodes[nGoCheck].Started = t0
	p = p.withCells(rs.cells(t0))
	before := ansi.Strip(p.View(rs, DefaultStyles(), t0))

	tests := []struct {
		id    string
		label string
	}{
		{nGuard, "guard"},
		{nGoCheck, "go-check"},
		{nTofu, "tofu-apply"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			out := p.selectNode(tt.id).View(rs, DefaultStyles(), t0)
			marked, reverse := markedText(out)
			assert.Equal(t, []string{tt.label}, marked, "only the label text is marked")
			assert.False(t, reverse, "no reverse video")
			assert.Equal(t, before, ansi.Strip(out), "the selection does not move the layout")
		})
	}
}

func TestGraphViewRunningSpinner(t *testing.T) {
	p, rs := deployPane(t, 200, 20)
	rs.nodes[nGuard].State = events.Running
	rs.nodes[nGuard].Started = t0
	p = p.withCells(rs.cells(t0))
	out := ansi.Strip(p.View(rs, DefaultStyles(), t0.Add(spinnerInterval)))
	assert.Contains(t, out, spinnerFrames[1]+" guard 0:00")
	assert.NotContains(t, out, "●")
}

func TestGraphViewWidthNeverExceedsPane(t *testing.T) {
	for _, w := range []int{10, 25, 40, 80, 120} {
		p, rs := deployPane(t, w, 8)
		p = p.selectNode(nTofu)
		out := p.View(rs, DefaultStyles(), t0)
		for _, line := range splitLines(out) {
			assert.LessOrEqual(t, ansi.StringWidth(line), w, "width %d: %q", w, ansi.Strip(line))
		}
		assert.Len(t, splitLines(out), p.rows())
	}
}

func TestGraphViewGolden(t *testing.T) {
	p, rs := deployPane(t, 120, 10)
	for _, id := range []string{nGuard, nGoCheck, nUICheck, nDBBackup} {
		rs.nodes[id].State = events.Succeeded
		rs.nodes[id].Started = t0
		rs.nodes[id].Finished = t0.Add(20 * time.Second)
	}
	rs.nodes[nRelease].State = events.Running
	rs.nodes[nRelease].Started = t0.Add(20 * time.Second)
	p = p.withCells(rs.cells(t0.Add(111 * time.Second))).selectNode(nRelease)
	golden.RequireEqual(t, ansi.Strip(p.View(rs, DefaultStyles(), t0)))
}
