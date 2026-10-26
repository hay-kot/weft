package tui

import (
	"maps"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
)

// direction is a selection move on screen.
type direction int

const (
	dirLeft direction = iota
	dirRight
	dirUp
	dirDown
)

// graphPane shows the laid-out DAG and owns the node selection.
type graphPane struct {
	graph layout.Graph
	cells map[string]layout.Cell
	// orient is the requested orientation. Auto falls back to vertical
	// when the horizontal layout is wider than the pane.
	orient   layout.Orientation
	grid     *layout.Grid
	selected string
	width    int
	// height is the most rows the pane may use; it uses fewer when the
	// graph is shorter.
	height int
	offX   int
	offY   int
}

func newGraphPane(g layout.Graph, cells map[string]layout.Cell) graphPane {
	selected := ""
	if len(g.Nodes) > 0 {
		selected = g.Nodes[0].ID
	}
	p := graphPane{
		graph: g, cells: cells, orient: layout.Auto, grid: nil, selected: selected,
		width: 0, height: 0, offX: 0, offY: 0,
	}
	return p.relayout()
}

func (p graphPane) relayout() graphPane {
	cells := p.cells
	p.grid = layout.Layout(p.graph, layout.Options{
		Width:        p.width,
		Orientation:  p.orient,
		Cell:         func(id string) layout.Cell { return cells[id] },
		MaxCellWidth: 0,
	})
	return p.scrolledIntoView()
}

// withCells relayouts when any cell's text changed.
func (p graphPane) withCells(cells map[string]layout.Cell) graphPane {
	if maps.Equal(p.cells, cells) {
		return p
	}
	p.cells = cells
	return p.relayout()
}

// withSize sets the pane's width and its maximum height.
func (p graphPane) withSize(width, height int) graphPane {
	if p.width == width && p.height == height {
		return p
	}
	p.width, p.height = max(0, width), max(0, height)
	return p.relayout()
}

// rows returns how many rows the pane draws.
func (p graphPane) rows() int {
	return min(p.grid.Height, p.height)
}

// fullHeight returns the rows the whole graph needs at the current width.
func (p graphPane) fullHeight() int {
	return p.grid.Height
}

// toggleLayout switches between horizontal and vertical. The choice sticks
// across resizes.
func (p graphPane) toggleLayout() graphPane {
	if p.grid.Orientation == layout.Horizontal {
		p.orient = layout.Vertical
	} else {
		p.orient = layout.Horizontal
	}
	return p.relayout()
}

func (p graphPane) selectNode(id string) graphPane {
	if _, ok := p.grid.Boxes[id]; !ok {
		return p
	}
	p.selected = id
	return p.scrolledIntoView()
}

// Update handles selection and layout keys. It reports whether the key was
// one of them.
func (p graphPane) Update(msg tea.Msg, keys KeyMap) (graphPane, bool) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, false
	}
	switch {
	case key.Matches(k, keys.Left):
		return p.move(dirLeft), true
	case key.Matches(k, keys.Right):
		return p.move(dirRight), true
	case key.Matches(k, keys.Up):
		return p.move(dirUp), true
	case key.Matches(k, keys.Down):
		return p.move(dirDown), true
	case key.Matches(k, keys.Layout):
		return p.toggleLayout(), true
	}
	return p, false
}

func (p graphPane) move(d direction) graphPane {
	if id := p.neighbor(d); id != "" {
		return p.selectNode(id)
	}
	return p
}

// neighbor returns the node one step from the selection in direction d,
// or "" when there is none. Moving along the levels picks the node in the
// next level nearest the selection on the cross axis; moving across steps
// through the selection's own level.
func (p graphPane) neighbor(d direction) string {
	cur, ok := p.grid.Boxes[p.selected]
	if !ok {
		return ""
	}
	alongLevels := d == dirLeft || d == dirRight
	if p.grid.Orientation == layout.Vertical {
		alongLevels = !alongLevels
	}
	step := 1
	if d == dirLeft || d == dirUp {
		step = -1
	}

	if !alongLevels {
		ids := p.grid.Levels[cur.Level]
		i := cur.Index + step
		if i < 0 || i >= len(ids) {
			return ""
		}
		return ids[i]
	}

	l := cur.Level + step
	if l < 0 || l >= len(p.grid.Levels) {
		return ""
	}
	best, bestDist := "", math.Inf(1)
	for _, id := range p.grid.Levels[l] {
		if dist := math.Abs(p.cross(p.grid.Boxes[id]) - p.cross(cur)); dist < bestDist {
			best, bestDist = id, dist
		}
	}
	return best
}

// cross returns a box's center on the axis across the levels.
func (p graphPane) cross(b layout.Box) float64 {
	if p.grid.Orientation == layout.Vertical {
		return float64(b.X) + float64(b.Width)/2
	}
	return float64(b.Y)
}

// scrolledIntoView moves the viewport the least needed to show the
// selected node, then clamps it to the grid.
func (p graphPane) scrolledIntoView() graphPane {
	if b, ok := p.grid.Boxes[p.selected]; ok {
		if b.X < p.offX {
			p.offX = b.X
		}
		if end := b.X + b.Width; end > p.offX+p.width {
			p.offX = end - p.width
		}
		if b.Y < p.offY {
			p.offY = b.Y
		}
		if b.Y >= p.offY+p.rows() {
			p.offY = b.Y - p.rows() + 1
		}
	}
	p.offX = min(max(0, p.offX), max(0, p.grid.Width-p.width))
	p.offY = min(max(0, p.offY), max(0, p.grid.Height-p.rows()))
	return p
}

// glyphClass says how one grid glyph is drawn.
type glyphClass struct {
	kind     glyphKind
	state    events.State
	selected bool
}

type glyphKind int

const (
	glyphBlank glyphKind = iota
	glyphEdge
	glyphEdgeActive
	glyphIcon
	glyphLabel
	glyphSuffix
)

// classify decides how the glyph at x, y is drawn.
func (p graphPane) classify(x, y int, gl layout.Glyph, rs *runState, active map[int]bool) glyphClass {
	if gl.Node != "" {
		st := rs.state(gl.Node)
		sel := gl.Node == p.selected
		b := p.grid.Boxes[gl.Node]
		off := x - b.X
		cell := p.cells[gl.Node]
		suffixStart := b.Width - ansi.StringWidth(cell.Suffix)
		labelStart := ansi.StringWidth(cell.Prefix)
		labelEnd := min(labelStart+ansi.StringWidth(cell.Label), suffixStart)
		switch {
		case y == b.Y && off == 0:
			return glyphClass{kind: glyphIcon, state: st, selected: false}
		case off >= suffixStart:
			return glyphClass{kind: glyphSuffix, state: st, selected: false}
		default:
			// Only the label text carries the selection, so the marker
			// never runs under the padding or into the icon's state color.
			onText := off >= labelStart && off < labelEnd
			return glyphClass{kind: glyphLabel, state: st, selected: sel && onText}
		}
	}
	for _, e := range gl.Edges {
		if active[e] {
			return glyphClass{kind: glyphEdgeActive, state: "", selected: false}
		}
	}
	if len(gl.Edges) > 0 {
		return glyphClass{kind: glyphEdge, state: "", selected: false}
	}
	return glyphClass{kind: glyphBlank, state: "", selected: false}
}

func (c glyphClass) style(s Styles) lipgloss.Style {
	var st lipgloss.Style
	switch c.kind {
	case glyphEdge:
		st = s.Edge
	case glyphEdgeActive:
		st = s.EdgeActive
	case glyphIcon:
		st = s.State(c.state)
	case glyphLabel:
		switch {
		case c.selected:
			st = s.Selected
		case c.state == events.Pending || c.state == events.Ready:
			st = s.Dim
		default:
			st = lipgloss.NewStyle()
		}
	case glyphSuffix:
		st = s.Dim
	case glyphBlank:
		return lipgloss.NewStyle()
	}
	return st
}

// View draws the visible part of the graph, one line per row.
func (p graphPane) View(rs *runState, s Styles, now time.Time) string {
	active := map[int]bool{}
	for _, e := range p.grid.EdgesOf(p.selected) {
		active[e] = true
	}
	frame := spinnerFrame(now)
	right := min(p.grid.Width, p.offX+p.width)

	lines := make([]string, 0, p.rows())
	for y := p.offY; y < p.offY+p.rows(); y++ {
		var line, run strings.Builder
		cur := glyphClass{kind: glyphBlank, state: "", selected: false}
		flush := func() {
			if run.Len() > 0 {
				line.WriteString(cur.style(s).Render(run.String()))
				run.Reset()
			}
		}
		for x := p.offX; x < right; x++ {
			gl := p.grid.At(x, y)
			text := gl.Text
			switch {
			case text == "" && x == p.offX:
				// The second half of a wide glyph cut off by scrolling.
				text = " "
			case text == "":
				continue
			case ansi.StringWidth(text) > right-x:
				text = " "
			}
			c := p.classify(x, y, gl, rs, active)
			if c.kind == glyphIcon && c.state == events.Running {
				text = frame
			}
			if c != cur {
				flush()
				cur = c
			}
			run.WriteString(text)
		}
		flush()
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return strings.Join(lines, "\n")
}
