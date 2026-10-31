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
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

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

// vGap is the space between cells in one row of a vertical layout.
const vGap = 2

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
	edges []int // for dummies: indexes into builder.edges
	decl  int   // declaration index of the node, or of the dummy's source
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
		h := b.renderHorizontal()
		if opts.Width <= 0 || h.Width <= opts.Width {
			return h
		}
		orient = Vertical
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
			id: n.ID, real: true, edges: nil, decl: decl[n.ID], level: level[n.ID],
			text: "", width: 0, preds: nil, succs: nil, pos: 0,
		}
		b.byID[n.ID] = it
		b.levels[it.level] = append(b.levels[it.level], it)
	}

	// Skip edges from one source share a dummy per level. A dummy each would
	// add a row of line per edge, and those rows run level with unrelated
	// nodes and join their trunks, which draws dependencies that do not exist.
	type dummyKey struct {
		src   *item
		level int
	}
	dummies := map[dummyKey]*item{}
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
				key := dummyKey{src: src, level: l}
				d, shared := dummies[key]
				if !shared {
					d = &item{
						id: "", real: false, edges: nil, decl: src.decl, level: l,
						text: "", width: 1, preds: nil, succs: nil, pos: 0,
					}
					dummies[key] = d
					b.levels[l] = append(b.levels[l], d)
				}
				d.edges = append(d.edges, ei)
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
	if !slices.Contains(from.succs, to) {
		from.succs = append(from.succs, to)
		to.preds = append(to.preds, from)
	}
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
//
// Connected components are placed separately and stacked. Rounding moves a
// component a little on every sweep, and an item with no neighbors stays put,
// so placing them together opens gaps that grow with the sweeps.
func (b *builder) place(size func(*item) int, gap int) {
	comps := b.components()
	next := 0
	for _, levels := range comps {
		placeComponent(levels, size, gap)
		end := next
		for _, lv := range levels {
			for _, it := range lv {
				it.pos += next
				end = max(end, it.pos+size(it)+gap)
			}
		}
		next = end
	}
	for l := range b.levels {
		b.levels[l] = b.levels[l][:0]
		for _, levels := range comps {
			b.levels[l] = append(b.levels[l], levels[l]...)
		}
	}
}

// components splits the levels by connected component, ordered by each
// component's first declared node. Items keep their order within a level.
func (b *builder) components() [][][]*item {
	var items []*item
	for _, lv := range b.levels {
		items = append(items, lv...)
	}
	slices.SortStableFunc(items, func(a, c *item) int { return cmp.Compare(a.decl, c.decl) })

	comp := map[*item]int{}
	var mark func(it *item, id int)
	mark = func(it *item, id int) {
		if _, seen := comp[it]; seen {
			return
		}
		comp[it] = id
		for _, n := range it.preds {
			mark(n, id)
		}
		for _, n := range it.succs {
			mark(n, id)
		}
	}
	count := 0
	for _, it := range items {
		if _, seen := comp[it]; !seen {
			mark(it, count)
			count++
		}
	}

	out := make([][][]*item, count)
	for i := range out {
		out[i] = make([][]*item, len(b.levels))
	}
	for l, lv := range b.levels {
		for _, it := range lv {
			out[comp[it]][l] = append(out[comp[it]][l], it)
		}
	}
	return out
}

// placeComponent positions one component with its lowest item at zero.
func placeComponent(levels [][]*item, size func(*item) int, gap int) {
	for _, lv := range levels {
		pos := 0
		for _, it := range lv {
			it.pos = pos
			pos += size(it) + gap
		}
	}
	preds := func(it *item) []*item { return it.preds }
	succs := func(it *item) []*item { return it.succs }
	for l := 1; l < len(levels); l++ {
		align(levels[l], preds, size, gap)
	}
	for l := len(levels) - 2; l >= 0; l-- {
		align(levels[l], succs, size, gap)
	}
	for l := 1; l < len(levels); l++ {
		align(levels[l], preds, size, gap)
	}

	low := math.MaxInt
	for _, lv := range levels {
		for _, it := range lv {
			low = min(low, it.pos)
		}
	}
	for _, lv := range levels {
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

// text walks grapheme clusters, not runes, because cells are sized by
// grapheme width: a ZWJ emoji is one wide glyph, not several.
func (c *canvas) text(x, y int, s, node string) {
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		cluster := gr.Str()
		w := ansi.StringWidth(cluster)
		if w == 0 {
			continue
		}
		c.setNode(x, y, cluster, node)
		for i := 1; i < w; i++ {
			c.setNode(x+i, y, "", node)
		}
		x += w
	}
}

func (c *canvas) setNode(x, y int, text, node string) {
	if y < 0 || y >= len(c.glyphs) || x < 0 || x >= len(c.glyphs[y]) {
		return
	}
	c.set(x, y, text)
	c.glyphs[y][x].Node = node
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

// bundle is a set of sources that all feed the same targets. One line joins
// them, because every source really does reach every target. Sources with
// different targets get different bundles, or the shared line would draw
// edges that do not exist.
type bundle struct {
	segs     []segment
	sources  []int
	targets  []int
	lo, hi   int
	track    int
	straight bool
}

// routeGutter groups the segments between two levels into bundles and gives
// each a track, the line it runs along between the levels. A source's line
// must end before the line of a target on the same row starts, so bundles
// that would swap rows both ways cannot be separated and share one line.
func routeGutter(segs []segment, cross func(*item) int) ([]*bundle, int) {
	bundles := groupBundles(segs, cross)
	markStraight(bundles)
	for {
		tracks, stuck := assignTracks(bundles)
		if len(stuck) == 0 {
			return bundles, max(1, tracks)
		}
		bundles = mergeBundles(bundles, stuck)
	}
}

// mergeBundles replaces the stuck bundles with one bundle that holds them all.
func mergeBundles(bundles, stuck []*bundle) []*bundle {
	merged := &bundle{lo: stuck[0].lo, hi: stuck[0].hi}
	out := []*bundle{merged}
	for _, bl := range bundles {
		if !slices.Contains(stuck, bl) {
			out = append(out, bl)
			continue
		}
		merged.segs = append(merged.segs, bl.segs...)
		merged.sources = append(merged.sources, bl.sources...)
		for _, t := range bl.targets {
			if !slices.Contains(merged.targets, t) {
				merged.targets = append(merged.targets, t)
			}
		}
		merged.lo, merged.hi = min(merged.lo, bl.lo), max(merged.hi, bl.hi)
	}
	return out
}

func groupBundles(segs []segment, cross func(*item) int) []*bundle {
	targets := map[int][]int{}
	var sources []int
	for _, s := range segs {
		from, to := cross(s.from), cross(s.to)
		if _, seen := targets[from]; !seen {
			sources = append(sources, from)
		}
		if !slices.Contains(targets[from], to) {
			targets[from] = append(targets[from], to)
		}
	}
	byKey := map[string]*bundle{}
	bySource := map[int]*bundle{}
	var out []*bundle
	for _, from := range sources {
		slices.Sort(targets[from])
		key := fmt.Sprint(targets[from])
		bl := byKey[key]
		if bl == nil {
			bl = &bundle{targets: targets[from], lo: from, hi: from}
			for _, t := range targets[from] {
				bl.lo, bl.hi = min(bl.lo, t), max(bl.hi, t)
			}
			byKey[key] = bl
			out = append(out, bl)
		}
		bl.sources = append(bl.sources, from)
		bl.lo, bl.hi = min(bl.lo, from), max(bl.hi, from)
		bySource[from] = bl
	}
	for _, s := range segs {
		bl := bySource[cross(s.from)]
		bl.segs = append(bl.segs, s)
	}
	return out
}

// markStraight finds lines that run level from one source to one target and
// need no track, unless another bundle joins that target's row.
func markStraight(bundles []*bundle) {
	for _, bl := range bundles {
		bl.straight = len(bl.sources) == 1 && len(bl.targets) == 1 && bl.sources[0] == bl.targets[0]
		if !bl.straight {
			continue
		}
		for _, other := range bundles {
			if other != bl && slices.Contains(other.targets, bl.targets[0]) {
				bl.straight = false
			}
		}
	}
}

// assignTracks returns the track count, or the bundles caught in a cycle of
// row constraints. Bundles share a track when their spans do not touch.
func assignTracks(bundles []*bundle) (int, []*bundle) {
	before := func(a, c *bundle) bool {
		return a != c && slices.ContainsFunc(a.sources, func(s int) bool { return slices.Contains(c.targets, s) })
	}
	var todo, done []*bundle
	for _, bl := range bundles {
		if !bl.straight {
			todo = append(todo, bl)
		}
	}
	tracks := 0
	for len(todo) > 0 {
		next := slices.IndexFunc(todo, func(bl *bundle) bool {
			return !slices.ContainsFunc(todo, func(a *bundle) bool { return before(a, bl) })
		})
		if next < 0 {
			return 0, todo
		}
		bl := todo[next]
		todo = slices.Delete(todo, next, next+1)
		bl.track = 0
		for _, a := range done {
			if before(a, bl) {
				bl.track = max(bl.track, a.track+1)
			}
		}
		for slices.ContainsFunc(done, func(a *bundle) bool {
			return a.track == bl.track && a.lo <= bl.hi && bl.lo <= a.hi
		}) {
			bl.track++
		}
		done = append(done, bl)
		tracks = max(tracks, bl.track+1)
	}
	return tracks, nil
}

// trackCell is one cell of a gutter's tracks, in the gutter's own frame:
// before and after run along the flow, lower and higher across it.
type trackCell struct {
	before, after, lower, higher bool
	trunk, pass                  bool
	edges                        []int
}

func paintTracks(bundles []*bundle, tracks int, cross func(*item) int) map[[2]int]*trackCell {
	cells := map[[2]int]*trackCell{}
	at := func(track, c, edge int) *trackCell {
		k := [2]int{track, c}
		if cells[k] == nil {
			cells[k] = &trackCell{}
		}
		if !slices.Contains(cells[k].edges, edge) {
			cells[k].edges = append(cells[k].edges, edge)
		}
		return cells[k]
	}
	for _, bl := range bundles {
		for _, s := range bl.segs {
			from, to := cross(s.from), cross(s.to)
			if bl.straight {
				for t := range tracks {
					at(t, from, s.edge).pass = true
				}
				continue
			}
			lo, hi := min(from, to), max(from, to)
			for c := lo; c <= hi; c++ {
				cell := at(bl.track, c, s.edge)
				cell.trunk = true
				cell.lower = cell.lower || c > lo
				cell.higher = cell.higher || c < hi
			}
			at(bl.track, from, s.edge).before = true
			at(bl.track, to, s.edge).after = true
			for t := range bl.track {
				at(t, from, s.edge).pass = true
			}
			for t := bl.track + 1; t < tracks; t++ {
				at(t, to, s.edge).pass = true
			}
		}
	}
	return cells
}

// glyph returns the box glyph for the cell. A line that crosses a bundle it
// has no part in draws as the bundle's own vertical line, not as a ┼, because
// ┼ means the lines join.
func (c *trackCell) glyph(vertical bool) string {
	switch {
	case !c.trunk && vertical:
		return "│"
	case !c.trunk:
		return "─"
	case c.pass && !c.before && !c.after:
		return "│"
	}
	before, after := c.before || c.pass, c.after || c.pass
	if vertical {
		return junction(before, after, c.lower, c.higher)
	}
	return junction(c.lower, c.higher, before, after)
}

func (b *builder) segmentsFrom(l int) []segment {
	var segs []segment
	for _, s := range b.segments {
		if s.from.level == l {
			segs = append(segs, s)
		}
	}
	return segs
}

func (b *builder) renderHorizontal() *Grid {
	b.place(func(*item) int { return 1 }, 0)
	pos := func(it *item) int { return it.pos }

	type gutter struct {
		bundles []*bundle
		tracks  int
	}
	gutters := make([]gutter, max(0, len(b.levels)-1))
	colX := make([]int, len(b.levels))
	colW := make([]int, len(b.levels))
	x, height := 0, 0
	for l, lv := range b.levels {
		colX[l] = x
		colW[l] = levelWidth(lv)
		x += colW[l]
		if l < len(gutters) {
			bundles, tracks := routeGutter(b.segmentsFrom(l), pos)
			gutters[l] = gutter{bundles, tracks}
			// Gutter: " ─", the tracks, "─▸ ".
			x += tracks + 5
		}
		for _, it := range lv {
			height = max(height, it.pos+1)
		}
	}
	c := newCanvas(x, height)

	for l, lv := range b.levels {
		for _, it := range lv {
			if it.real {
				c.text(colX[l], it.pos, it.text, it.id)
				continue
			}
			for dx := range colW[l] {
				c.set(colX[l]+dx, it.pos, "─", it.edges...)
			}
		}
	}

	for l, gt := range gutters {
		gx := colX[l] + colW[l]
		first := gx + 2
		end := first + gt.tracks
		for _, bl := range gt.bundles {
			for _, s := range bl.segs {
				start := gx
				if s.from.real {
					start = colX[l] + s.from.width + 1
				}
				for xx := start; xx < first; xx++ {
					c.set(xx, s.from.pos, "─", s.edge)
				}
				c.set(end, s.to.pos, "─", s.edge)
				if s.to.real {
					c.set(end+1, s.to.pos, "▸", s.edge)
				} else {
					c.set(end+1, s.to.pos, "─", s.edge)
					c.set(end+2, s.to.pos, "─", s.edge)
				}
			}
		}
		for k, cell := range paintTracks(gt.bundles, gt.tracks, pos) {
			c.set(first+k[0], k[1], cell.glyph(false), cell.edges...)
		}
	}

	g := b.grid(c, Horizontal)
	b.boxes(g, func(it *item) (int, int) { return colX[it.level], it.pos })
	return g
}

func (b *builder) renderVertical() *Grid {
	b.place(func(it *item) int { return it.width }, vGap)
	center := func(it *item) int { return it.pos + (it.width-1)/2 }

	width := 0
	for _, lv := range b.levels {
		for _, it := range lv {
			width = max(width, it.pos+it.width)
		}
	}
	type gutter struct {
		bundles []*bundle
		tracks  int
	}
	gutters := make([]gutter, max(0, len(b.levels)-1))
	rows := make([]int, len(b.levels))
	y := 0
	for l := range b.levels {
		rows[l] = y
		y++
		if l < len(gutters) {
			bundles, tracks := routeGutter(b.segmentsFrom(l), center)
			gutters[l] = gutter{bundles, tracks}
			// Gutter: stems, the tracks, arrowheads.
			y += tracks + 2
		}
	}
	c := newCanvas(width, y)

	for l, lv := range b.levels {
		for _, it := range lv {
			if it.real {
				c.text(it.pos, rows[l], it.text, it.id)
			} else {
				c.set(it.pos, rows[l], "│", it.edges...)
			}
		}
	}

	for l, gt := range gutters {
		stem := rows[l] + 1
		first := stem + 1
		head := first + gt.tracks
		for _, bl := range gt.bundles {
			for _, s := range bl.segs {
				c.set(center(s.from), stem, "│", s.edge)
				if s.to.real {
					c.set(center(s.to), head, "▾", s.edge)
				} else {
					c.set(center(s.to), head, "│", s.edge)
				}
			}
		}
		for k, cell := range paintTracks(gt.bundles, gt.tracks, center) {
			c.set(k[1], first+k[0], cell.glyph(true), cell.edges...)
		}
	}

	g := b.grid(c, Vertical)
	b.boxes(g, func(it *item) (int, int) { return it.pos, rows[it.level] })
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
