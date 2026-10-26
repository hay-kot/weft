// Package layout turns a workflow DAG into a grid of text cells: nodes laid
// out in levels, with edges drawn in box glyphs between them.
//
// Layout is a pure function of the graph, the cell text, and the options,
// so the same input always gives the same grid. The TUI renders the grid
// with styles; `weft graph` prints it as plain text.
//
// In the horizontal orientation each level is a column and edges run left
// to right. In the vertical orientation each level is a row and edges run
// top to bottom. Edges that skip levels pass through dummy cells in the
// levels between (Sugiyama-style), drawn as straight lines.
package layout

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/workflow"
)

// Orientation picks the direction of the layout.
type Orientation int

// Orientations. Auto is horizontal unless that is wider than Options.Width.
const (
	Auto Orientation = iota
	Horizontal
	Vertical
)

func (o Orientation) String() string {
	switch o {
	case Horizontal:
		return "horizontal"
	case Vertical:
		return "vertical"
	default:
		return "auto"
	}
}

// DefaultMaxCellWidth caps a node cell's display width.
const DefaultMaxCellWidth = 20

const (
	// hGutter is the width between columns: " ─┼─▸ ".
	hGutter = 6
	// vGutter is the height between rows: stems, the bus, arrowheads.
	vGutter = 3
	// vGap is the space between cells in one row of a vertical layout.
	vGap = 2
)

// Node is one node of the graph to lay out.
type Node struct {
	ID    string
	Needs []string
}

// Graph is a DAG in declaration order. Needs that name no node are
// ignored; the graph must be acyclic.
type Graph struct {
	Nodes []Node
}

// FromWorkflow returns the graph of a validated workflow.
func FromWorkflow(wf *workflow.Workflow) Graph {
	nodes := make([]Node, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		nodes = append(nodes, Node{ID: n.ID, Needs: slices.Clone(n.Needs)})
	}
	return Graph{Nodes: nodes}
}

// Cell is the text of one node. Layout pads Label to the widest label in
// the node's level, so suffixes such as durations line up, and truncates it
// with "…" to keep the cell within the maximum width.
type Cell struct {
	Prefix string
	Label  string
	Suffix string
}

// Options configures Layout.
type Options struct {
	// Width is the space available. With Auto, a horizontal layout wider
	// than Width falls back to vertical. Zero means unlimited.
	Width       int
	Orientation Orientation
	// Cell returns the text for a node. Nil uses the node ID as the label.
	Cell func(id string) Cell
	// MaxCellWidth caps each node cell; zero means DefaultMaxCellWidth.
	MaxCellWidth int
}

// Edge is one dependency: From must finish before To.
type Edge struct {
	From string
	To   string
}

// Box is where a node's cell sits in the grid.
type Box struct {
	ID string
	// Level is the node's column (horizontal) or row (vertical) of levels.
	Level int
	// Index is the node's position within its level, counting only real
	// nodes, in display order.
	Index int
	X, Y  int
	// Width is the cell's display width.
	Width int
}

// Glyph is one grid cell.
type Glyph struct {
	// Text is one grapheme of display width 1 or 2, or empty for the
	// second column of a wide grapheme.
	Text string
	// Node is set when the glyph is part of a node's cell.
	Node string
	// Edges lists indexes into Grid.Edges of the edges drawn through this
	// glyph. Trunk glyphs can belong to several.
	Edges []int
}

// Grid is a laid-out graph.
type Grid struct {
	Width, Height int
	Orientation   Orientation
	// Edges lists every edge of the graph, in declaration order.
	Edges []Edge
	// Levels holds the real node IDs of each level in display order.
	Levels [][]string
	Boxes  map[string]Box

	glyphs [][]Glyph
}

// At returns the glyph at x, y; outside the grid it returns a space.
func (g *Grid) At(x, y int) Glyph {
	if y < 0 || y >= len(g.glyphs) || x < 0 || x >= len(g.glyphs[y]) {
		return Glyph{Text: " ", Node: "", Edges: nil}
	}
	return g.glyphs[y][x]
}

// Lines returns the grid as plain text, one string per row, with trailing
// spaces removed.
func (g *Grid) Lines() []string {
	lines := make([]string, len(g.glyphs))
	for y, row := range g.glyphs {
		var b strings.Builder
		for _, gl := range row {
			b.WriteString(gl.Text)
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	return lines
}

// String returns Lines joined with newlines.
func (g *Grid) String() string {
	return strings.Join(g.Lines(), "\n")
}

// EdgesOf returns the indexes of the edges that start or end at id.
func (g *Grid) EdgesOf(id string) []int {
	var out []int
	for i, e := range g.Edges {
		if e.From == id || e.To == id {
			out = append(out, i)
		}
	}
	return out
}

// item is a real node or a dummy that carries a long edge through a level.
type item struct {
	id    string
	real  bool
	edge  int // for dummies: index into edges
	decl  int // declaration index of the node, or of the dummy's source
	level int
	text  string // rendered cell, real items only
	width int

	preds, succs []*item
	pos          int // cross-axis start: row (horizontal) or column (vertical)
}

// segment is one hop of an edge between adjacent levels.
type segment struct {
	from, to *item
	edge     int
}

// Layout lays out g. The result depends only on its arguments.
func Layout(g Graph, opts Options) *Grid {
	if opts.MaxCellWidth <= 0 {
		opts.MaxCellWidth = DefaultMaxCellWidth
	}
	if opts.Cell == nil {
		opts.Cell = func(id string) Cell { return Cell{Prefix: "", Label: id, Suffix: ""} }
	}

	b := build(g)
	b.order()
	b.cellText(opts)

	orient := opts.Orientation
	if orient == Auto {
		orient = Horizontal
		if opts.Width > 0 && b.horizontalWidth() > opts.Width {
			orient = Vertical
		}
	}

	if orient == Vertical {
		return b.renderVertical()
	}
	return b.renderHorizontal()
}

type builder struct {
	levels   [][]*item
	segments []segment
	edges    []Edge
	byID     map[string]*item
}

func build(g Graph) *builder {
	b := &builder{levels: nil, segments: nil, edges: nil, byID: map[string]*item{}}
	decl := map[string]int{}
	for i, n := range g.Nodes {
		decl[n.ID] = i
	}

	level := map[string]int{}
	var visit func(id string, depth int) int
	byNode := map[string]Node{}
	for _, n := range g.Nodes {
		byNode[n.ID] = n
	}
	visit = func(id string, depth int) int {
		if l, ok := level[id]; ok {
			return l
		}
		l := 0
		if depth <= len(g.Nodes) {
			for _, need := range byNode[id].Needs {
				if _, ok := byNode[need]; ok {
					l = max(l, visit(need, depth+1)+1)
				}
			}
		}
		level[id] = l
		return l
	}

	maxLevel := -1
	for _, n := range g.Nodes {
		maxLevel = max(maxLevel, visit(n.ID, 0))
	}
	b.levels = make([][]*item, maxLevel+1)
	for _, n := range g.Nodes {
		it := &item{
			id: n.ID, real: true, edge: -1, decl: decl[n.ID], level: level[n.ID],
			text: "", width: 0, preds: nil, succs: nil, pos: 0,
		}
		b.byID[n.ID] = it
		b.levels[it.level] = append(b.levels[it.level], it)
	}

	for _, n := range g.Nodes {
		for _, need := range n.Needs {
			src, ok := b.byID[need]
			if !ok {
				continue
			}
			dst := b.byID[n.ID]
			ei := len(b.edges)
			b.edges = append(b.edges, Edge{From: need, To: n.ID})
			prev := src
			for l := src.level + 1; l < dst.level; l++ {
				d := &item{
					id: "", real: false, edge: ei, decl: src.decl, level: l,
					text: "", width: 1, preds: nil, succs: nil, pos: 0,
				}
				b.levels[l] = append(b.levels[l], d)
				b.link(prev, d, ei)
				prev = d
			}
			b.link(prev, dst, ei)
		}
	}

	// Initial order: declaration order, with each dummy right after the
	// real nodes declared before its source.
	for _, lv := range b.levels {
		slices.SortStableFunc(lv, func(a, c *item) int {
			return cmp.Or(cmp.Compare(a.decl, c.decl), cmp.Compare(boolInt(!a.real), boolInt(!c.real)))
		})
	}
	return b
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (b *builder) link(from, to *item, edge int) {
	from.succs = append(from.succs, to)
	to.preds = append(to.preds, from)
	b.segments = append(b.segments, segment{from: from, to: to, edge: edge})
}

// order runs barycenter sweeps to reduce crossings: down (by parents), up
// (by children), then down again so dependencies settle left to right.
// Sorting is stable, so ties keep the current order, which starts as
// declaration order.
func (b *builder) order() {
	index := func() map[*item]int {
		m := map[*item]int{}
		for _, lv := range b.levels {
			for i, it := range lv {
				m[it] = i
			}
		}
		return m
	}
	sweep := func(l int, neighbors func(*item) []*item) {
		idx := index()
		keys := map[*item]float64{}
		for _, it := range b.levels[l] {
			ns := neighbors(it)
			if len(ns) == 0 {
				keys[it] = float64(idx[it])
				continue
			}
			sum := 0
			for _, n := range ns {
				sum += idx[n]
			}
			keys[it] = float64(sum) / float64(len(ns))
		}
		slices.SortStableFunc(b.levels[l], func(a, c *item) int {
			return cmp.Compare(keys[a], keys[c])
		})
	}
	preds := func(it *item) []*item { return it.preds }
	succs := func(it *item) []*item { return it.succs }
	for l := 1; l < len(b.levels); l++ {
		sweep(l, preds)
	}
	for l := len(b.levels) - 2; l >= 0; l-- {
		sweep(l, succs)
	}
	for l := 1; l < len(b.levels); l++ {
		sweep(l, preds)
	}
}

// cellText renders every real node's cell, padding labels within a level
// so prefixes and suffixes line up.
func (b *builder) cellText(opts Options) {
	cells := map[string]Cell{}
	for _, lv := range b.levels {
		labelW := 0
		for _, it := range lv {
			if !it.real {
				continue
			}
			c := opts.Cell(it.id)
			room := max(1, opts.MaxCellWidth-ansi.StringWidth(c.Prefix)-ansi.StringWidth(c.Suffix))
			if ansi.StringWidth(c.Label) > room {
				c.Label = ansi.Truncate(c.Label, room, "…")
			}
			cells[it.id] = c
			labelW = max(labelW, ansi.StringWidth(c.Label))
		}
		for _, it := range lv {
			if !it.real {
				continue
			}
			c := cells[it.id]
			pad := labelW - ansi.StringWidth(c.Label)
			label := c.Label
			if c.Suffix != "" {
				label += strings.Repeat(" ", pad)
			}
			it.text = c.Prefix + label + c.Suffix
			it.width = ansi.StringWidth(it.text)
		}
	}
}

func (b *builder) horizontalWidth() int {
	w := 0
	for _, lv := range b.levels {
		w += levelWidth(lv)
	}
	return w + hGutter*max(0, len(b.levels)-1)
}

func levelWidth(lv []*item) int {
	w := 1
	for _, it := range lv {
		if it.real {
			w = max(w, it.width)
		}
	}
	return w
}

// place assigns cross-axis positions. Each item wants to sit centered on
// its neighbors in the previous level (or the next, sweeping up); items
// keep their order and stay gap apart. The best positions under those
// constraints come from isotonic regression (pool adjacent violators),
// which keeps a fan-out centered on its parent instead of hanging below it.
func (b *builder) place(size func(*item) int, gap int) {
	for _, lv := range b.levels {
		pos := 0
		for _, it := range lv {
			it.pos = pos
			pos += size(it) + gap
		}
	}
	preds := func(it *item) []*item { return it.preds }
	succs := func(it *item) []*item { return it.succs }
	for l := 1; l < len(b.levels); l++ {
		align(b.levels[l], preds, size, gap)
	}
	for l := len(b.levels) - 2; l >= 0; l-- {
		align(b.levels[l], succs, size, gap)
	}
	for l := 1; l < len(b.levels); l++ {
		align(b.levels[l], preds, size, gap)
	}

	low := 0
	for _, lv := range b.levels {
		for _, it := range lv {
			low = min(low, it.pos)
		}
	}
	for _, lv := range b.levels {
		for _, it := range lv {
			it.pos -= low
		}
	}
}

func align(lv []*item, neighbors func(*item) []*item, size func(*item) int, gap int) {
	n := len(lv)
	offset := make([]int, n)
	target := make([]float64, n)
	acc := 0
	for i, it := range lv {
		offset[i] = acc
		acc += size(it) + gap
		desired := float64(it.pos)
		if ns := neighbors(it); len(ns) > 0 {
			sum := 0
			for _, nb := range ns {
				sum += nb.pos + (size(nb)-1)/2
			}
			desired = float64(sum)/float64(len(ns)) - float64((size(it)-1)/2)
		}
		target[i] = desired - float64(offset[i])
	}

	// Pool adjacent violators: merge neighboring blocks until block values
	// are non-decreasing. A block's value is its median target for an odd
	// count, so the middle child of a fan-out lines up exactly with its
	// parent, and its mean for an even count, so a parent of two sits
	// between them.
	type block struct{ first, count int }
	value := func(bl block) float64 {
		ts := slices.Clone(target[bl.first : bl.first+bl.count])
		if bl.count%2 == 1 {
			slices.Sort(ts)
			return ts[bl.count/2]
		}
		sum := 0.0
		for _, t := range ts {
			sum += t
		}
		return sum / float64(bl.count)
	}
	var blocks []block
	for i := range n {
		blocks = append(blocks, block{first: i, count: 1})
		for len(blocks) > 1 {
			last, prev := blocks[len(blocks)-1], blocks[len(blocks)-2]
			if value(prev) <= value(last) {
				break
			}
			blocks = append(blocks[:len(blocks)-2], block{first: prev.first, count: prev.count + last.count})
		}
	}
	for _, bl := range blocks {
		base := roundHalfDown(value(bl))
		for i := bl.first; i < bl.first+bl.count; i++ {
			lv[i].pos = base + offset[i]
		}
	}
}

// roundHalfDown rounds to the nearest integer, with halves going down, so
// an item between two parents sits with the upper (or left) one.
func roundHalfDown(f float64) int {
	return int(math.Ceil(f - 0.5))
}

// canvas is a grid under construction.
type canvas struct {
	glyphs [][]Glyph
}

func newCanvas(w, h int) *canvas {
	c := &canvas{glyphs: make([][]Glyph, h)}
	for y := range c.glyphs {
		row := make([]Glyph, w)
		for x := range row {
			row[x] = Glyph{Text: " ", Node: "", Edges: nil}
		}
		c.glyphs[y] = row
	}
	return c
}

func (c *canvas) set(x, y int, text string, edges ...int) {
	if y < 0 || y >= len(c.glyphs) || x < 0 || x >= len(c.glyphs[y]) {
		return
	}
	g := &c.glyphs[y][x]
	g.Text = text
	for _, e := range edges {
		if !slices.Contains(g.Edges, e) {
			g.Edges = append(g.Edges, e)
		}
	}
}

func (c *canvas) text(x, y int, s, node string) {
	for _, r := range s {
		w := ansi.StringWidth(string(r))
		if w == 0 {
			continue
		}
		c.set(x, y, string(r))
		c.glyphs[y][x].Node = node
		if w == 2 {
			c.set(x+1, y, "")
			c.glyphs[y][x+1].Node = node
		}
		x += w
	}
}

// junctions maps up|down|left|right bits to the box glyph that joins them.
var junctions = [16]string{
	" ", "│", "│", "│", // none, up, down, up+down
	"─", "┘", "┐", "┤", // left, +up, +down, +up+down
	"─", "└", "┌", "├", // right, +up, +down, +up+down
	"─", "┴", "┬", "┼", // left+right, +up, +down, +up+down
}

// junction returns the box glyph that joins the given directions.
func junction(up, down, left, right bool) string {
	return junctions[boolInt(up)|boolInt(down)<<1|boolInt(left)<<2|boolInt(right)<<3]
}

func (b *builder) grid(c *canvas, orient Orientation) *Grid {
	g := &Grid{
		Width:       0,
		Height:      len(c.glyphs),
		Orientation: orient,
		Edges:       b.edges,
		Levels:      make([][]string, len(b.levels)),
		Boxes:       map[string]Box{},
		glyphs:      c.glyphs,
	}
	if len(c.glyphs) > 0 {
		g.Width = len(c.glyphs[0])
	}
	for l, lv := range b.levels {
		for _, it := range lv {
			if it.real {
				g.Levels[l] = append(g.Levels[l], it.id)
			}
		}
	}
	return g
}

func (b *builder) renderHorizontal() *Grid {
	b.place(func(*item) int { return 1 }, 0)

	colX := make([]int, len(b.levels))
	colW := make([]int, len(b.levels))
	x, height := 0, 0
	for l, lv := range b.levels {
		colX[l] = x
		colW[l] = levelWidth(lv)
		x += colW[l] + hGutter
		for _, it := range lv {
			height = max(height, it.pos+1)
		}
	}
	width := max(0, x-hGutter)
	c := newCanvas(width, height)

	for l, lv := range b.levels {
		for _, it := range lv {
			if it.real {
				c.text(colX[l], it.pos, it.text, it.id)
				continue
			}
			for dx := range colW[l] {
				c.set(colX[l]+dx, it.pos, "─", it.edge)
			}
		}
	}

	// Gutter after level l: " ─┼─▸ ". Column 2 is the trunk.
	for l := 0; l+1 < len(b.levels); l++ {
		gx := colX[l] + colW[l]
		trunk := gx + 2
		var segs []segment
		for _, s := range b.segments {
			if s.from.level == l {
				segs = append(segs, s)
			}
		}
		for _, s := range segs {
			start := gx
			if s.from.real {
				start = colX[l] + s.from.width + 1
			}
			for xx := start; xx < trunk; xx++ {
				c.set(xx, s.from.pos, "─", s.edge)
			}
			c.set(trunk+1, s.to.pos, "─", s.edge)
			if s.to.real {
				c.set(trunk+2, s.to.pos, "▸", s.edge)
			} else {
				c.set(trunk+2, s.to.pos, "─", s.edge)
				c.set(trunk+3, s.to.pos, "─", s.edge)
			}
		}
		for y := range height {
			var up, down, left, right bool
			var through []int
			for _, s := range segs {
				lo, hi := min(s.from.pos, s.to.pos), max(s.from.pos, s.to.pos)
				if y < lo || y > hi {
					continue
				}
				through = append(through, s.edge)
				left = left || s.from.pos == y
				right = right || s.to.pos == y
				up = up || y > lo
				down = down || y < hi
			}
			if len(through) > 0 {
				c.set(trunk, y, junction(up, down, left, right), through...)
			}
		}
	}

	g := b.grid(c, Horizontal)
	b.boxes(g, func(it *item) (int, int) { return colX[it.level], it.pos })
	return g
}

func (b *builder) renderVertical() *Grid {
	b.place(func(it *item) int { return it.width }, vGap)

	width := 0
	for _, lv := range b.levels {
		for _, it := range lv {
			width = max(width, it.pos+it.width)
		}
	}
	height := max(0, len(b.levels)*(1+vGutter)-vGutter)
	c := newCanvas(width, height)
	rowY := func(l int) int { return l * (1 + vGutter) }
	center := func(it *item) int { return it.pos + (it.width-1)/2 }

	for l, lv := range b.levels {
		for _, it := range lv {
			if it.real {
				c.text(it.pos, rowY(l), it.text, it.id)
			} else {
				c.set(it.pos, rowY(l), "│", it.edge)
			}
		}
	}

	for l := 0; l+1 < len(b.levels); l++ {
		stem, bus, head := rowY(l)+1, rowY(l)+2, rowY(l)+3
		var segs []segment
		for _, s := range b.segments {
			if s.from.level == l {
				segs = append(segs, s)
			}
		}
		for _, s := range segs {
			c.set(center(s.from), stem, "│", s.edge)
			if s.to.real {
				c.set(center(s.to), head, "▾", s.edge)
			} else {
				c.set(center(s.to), head, "│", s.edge)
			}
		}
		for x := range width {
			var up, down, left, right bool
			var through []int
			for _, s := range segs {
				xs, xt := center(s.from), center(s.to)
				lo, hi := min(xs, xt), max(xs, xt)
				if x < lo || x > hi {
					continue
				}
				through = append(through, s.edge)
				up = up || xs == x
				down = down || xt == x
				left = left || x > lo
				right = right || x < hi
			}
			if len(through) > 0 {
				c.set(x, bus, junction(up, down, left, right), through...)
			}
		}
	}

	g := b.grid(c, Vertical)
	b.boxes(g, func(it *item) (int, int) { return it.pos, rowY(it.level) })
	return g
}

func (b *builder) boxes(g *Grid, at func(*item) (int, int)) {
	for l, lv := range b.levels {
		idx := 0
		for _, it := range lv {
			if !it.real {
				continue
			}
			x, y := at(it)
			g.Boxes[it.id] = Box{ID: it.id, Level: l, Index: idx, X: x, Y: y, Width: it.width}
			idx++
		}
	}
}
