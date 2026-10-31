package layout_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/layout"
)

type dirs struct{ up, down, left, right bool }

var lineGlyphs = map[string]dirs{
	"─": {left: true, right: true},
	"│": {up: true, down: true},
	"┌": {down: true, right: true},
	"┐": {down: true, left: true},
	"└": {up: true, right: true},
	"┘": {up: true, left: true},
	"├": {up: true, down: true, right: true},
	"┤": {up: true, down: true, left: true},
	"┬": {down: true, left: true, right: true},
	"┴": {up: true, left: true, right: true},
	"┼": {up: true, down: true, left: true, right: true},
	"▸": {left: true},
	"▾": {up: true},
}

type point struct{ x, y int }

type violation struct{ category, detail string }

type cellMode int

const (
	cellPlain cellMode = iota
	cellTUI
	cellMixedLengths
	cellWide
	cellModeCount
)

func (m cellMode) String() string {
	return [...]string{"plain", "tui", "mixed", "wide"}[m]
}

func (m cellMode) cell(id string) layout.Cell {
	switch m {
	case cellTUI:
		return layout.Cell{Prefix: "○ ", Label: id, Suffix: " 0:05"}
	case cellMixedLengths:
		h := 0
		for _, r := range id {
			h = h*31 + int(r)
		}
		return layout.Cell{Prefix: "", Label: id + strings.Repeat("x", h%25), Suffix: ""}
	case cellWide:
		return layout.Cell{Prefix: "", Label: id + "部署", Suffix: ""}
	default:
		return layout.Cell{Prefix: "", Label: id, Suffix: ""}
	}
}

type scenario struct {
	orient layout.Orientation
	cells  cellMode
}

func render(g layout.Graph, sc scenario) *layout.Grid {
	return layout.Layout(g, layout.Options{Width: 0, Orientation: sc.orient, Cell: sc.cells.cell, MaxCellWidth: 0})
}

// exits returns where a node's outgoing lines start.
func exits(g *layout.Grid, id string) []point {
	b := g.Boxes[id]
	if g.Orientation == layout.Horizontal {
		return []point{{b.X + b.Width + 1, b.Y}}
	}
	out := make([]point, 0, b.Width)
	for x := b.X; x < b.X+b.Width; x++ {
		out = append(out, point{x, b.Y + 1})
	}
	return out
}

// target returns the node an arrowhead points into.
func target(g *layout.Grid, p point) string {
	if g.Orientation == layout.Horizontal {
		return g.At(p.x+2, p.y).Node
	}
	return g.At(p.x, p.y+1).Node
}

// flood follows the drawn lines from start the way a reader does: every
// junction joins everything that touches it, and lines only run with the
// flow of the layout, never back along it. It returns the nodes reached.
func flood(g *layout.Grid, start []point, allowed func(layout.Glyph) bool) map[string]bool {
	seen := map[point]bool{}
	reached := map[string]bool{}
	var walk func(p point)
	walk = func(p point) {
		if seen[p] {
			return
		}
		seen[p] = true
		gl := g.At(p.x, p.y)
		d := lineGlyphs[gl.Text]
		if gl.Text == "▸" || gl.Text == "▾" {
			if id := target(g, p); id != "" {
				reached[id] = true
			}
		}
		step := func(ok bool, n point, back func(dirs) bool) {
			if !ok {
				return
			}
			if n.y == p.y {
				n = pastCrossings(g, n, n.x-p.x)
			}
			ng := g.At(n.x, n.y)
			nd, isLine := lineGlyphs[ng.Text]
			if isLine && ng.Node == "" && back(nd) && allowed(ng) {
				walk(n)
			}
		}
		horizontal := g.Orientation == layout.Horizontal
		step(d.up && horizontal, point{p.x, p.y - 1}, func(d dirs) bool { return d.down })
		step(d.down, point{p.x, p.y + 1}, func(d dirs) bool { return d.up })
		step(d.left && !horizontal, point{p.x - 1, p.y}, func(d dirs) bool { return d.right })
		step(d.right, point{p.x + 1, p.y}, func(d dirs) bool { return d.left })
	}
	for _, p := range start {
		gl := g.At(p.x, p.y)
		d, isLine := lineGlyphs[gl.Text]
		if !isLine || gl.Node != "" || !allowed(gl) {
			continue
		}
		if g.Orientation == layout.Horizontal && !d.left {
			continue
		}
		if g.Orientation == layout.Vertical && !d.up {
			continue
		}
		walk(p)
	}
	return reached
}

// pastCrossings skips the vertical lines a horizontal line runs over.
func pastCrossings(g *layout.Grid, n point, dx int) point {
	for g.At(n.x, n.y).Text == "│" && g.At(n.x, n.y).Node == "" {
		n.x += dx
	}
	return n
}

func check(gr layout.Graph, sc scenario) []violation {
	var out []violation
	add := func(cat, format string, args ...any) {
		out = append(out, violation{cat, fmt.Sprintf(format, args...)})
	}
	g := render(gr, sc)

	if again := render(gr, sc); again.String() != g.String() {
		add("nondeterministic", "two layouts differ")
	}

	truth := map[string]map[string]bool{}
	ids := map[string]bool{}
	for _, n := range gr.Nodes {
		ids[n.ID] = true
		truth[n.ID] = map[string]bool{}
	}
	for _, n := range gr.Nodes {
		for _, need := range n.Needs {
			if ids[need] {
				truth[need][n.ID] = true
			}
		}
	}

	out = append(out, checkNodes(gr, g, sc)...)

	all := func(layout.Glyph) bool { return true }
	for _, n := range gr.Nodes {
		drawn := flood(g, exits(g, n.ID), all)
		for id := range drawn {
			if !truth[n.ID][id] {
				add(falseEdgeKind(gr, n.ID, id), "drawing shows %s -> %s", n.ID, id)
			}
		}
		for id := range truth[n.ID] {
			if !drawn[id] {
				add("missing-edge", "drawing lacks %s -> %s", n.ID, id)
			}
		}
	}

	// A selected node highlights the glyphs tagged with its edges, so the
	// tagged glyphs alone must form an unbroken path.
	for i, e := range g.Edges {
		tagged := func(gl layout.Glyph) bool { return slices.Contains(gl.Edges, i) }
		if !flood(g, exits(g, e.From), tagged)[e.To] {
			add("highlight-broken", "edge %s -> %s has no tagged path", e.From, e.To)
		}
	}
	out = append(out, checkTags(g)...)
	out = append(out, danglingLines(g)...)
	out = append(out, wastedSpace(gr, g)...)
	if sc.orient == layout.Vertical && g.Width > render(gr, scenario{layout.Horizontal, sc.cells}).Width {
		add("vertical-wider-than-horizontal", "vertical is %d wide", g.Width)
	}
	return out
}

func checkNodes(gr layout.Graph, g *layout.Grid, sc scenario) []violation {
	var out []violation
	add := func(cat, format string, args ...any) {
		out = append(out, violation{cat, fmt.Sprintf(format, args...)})
	}
	for _, n := range gr.Nodes {
		b, ok := g.Boxes[n.ID]
		if !ok {
			add("missing-node", "%s has no box", n.ID)
			continue
		}
		var text strings.Builder
		for x := b.X; x < b.X+b.Width; x++ {
			gl := g.At(x, b.Y)
			if gl.Node != n.ID {
				add("node-overwritten", "%s at %d,%d holds %q of node %q", n.ID, x, b.Y, gl.Text, gl.Node)
				break
			}
			if len(gl.Edges) > 0 {
				add("edge-through-node", "%s at %d,%d carries edges", n.ID, x, b.Y)
				break
			}
			text.WriteString(gl.Text)
		}
		c := sc.cells.cell(n.ID)
		if !strings.HasPrefix(text.String(), c.Prefix) || !strings.HasSuffix(text.String(), c.Suffix) {
			add("node-text-damaged", "%s drew %q", n.ID, text.String())
		}
		if ansi.StringWidth(c.Prefix+c.Label+c.Suffix) <= layout.DefaultMaxCellWidth &&
			!strings.Contains(text.String(), c.Label) {
			add("node-text-damaged", "%s drew %q", n.ID, text.String())
		}
	}
	return out
}

func checkTags(g *layout.Grid) []violation {
	var out []violation
	add := func(cat, format string, args ...any) {
		out = append(out, violation{cat, fmt.Sprintf(format, args...)})
	}
	for y := range g.Height {
		for x := range g.Width {
			gl := g.At(x, y)
			_, isLine := lineGlyphs[gl.Text]
			if isLine && gl.Node == "" && len(gl.Edges) == 0 {
				add("untagged-line", "%q at %d,%d has no edge", gl.Text, x, y)
			}
			if gl.Text == "▸" || gl.Text == "▾" {
				to := target(g, point{x, y})
				for _, ei := range gl.Edges {
					if g.Edges[ei].To != to {
						add("arrow-tag-wrong", "arrow into %q tagged %v", to, g.Edges[ei])
					}
				}
				if to == "" {
					add("arrow-to-nothing", "arrow at %d,%d", x, y)
				}
			}
		}
	}
	return out
}

func danglingLines(g *layout.Grid) []violation {
	var out []violation
	for y := range g.Height {
		for x := range g.Width {
			gl := g.At(x, y)
			d, isLine := lineGlyphs[gl.Text]
			if !isLine || gl.Node != "" {
				continue
			}
			joins := func(n point, back func(dirs) bool) bool {
				if n.y == y {
					n = pastCrossings(g, n, n.x-x)
				}
				ng := g.At(n.x, n.y)
				nd, ok := lineGlyphs[ng.Text]
				return ok && ng.Node == "" && back(nd)
			}
			bad := func(dir string) {
				out = append(out, violation{"dangling-line", fmt.Sprintf("%q at %d,%d open %s", gl.Text, x, y, dir)})
			}
			if d.up && !joins(point{x, y - 1}, func(d dirs) bool { return d.down }) && g.At(x, y-1).Node == "" {
				bad("up")
			}
			if d.down && !joins(point{x, y + 1}, func(d dirs) bool { return d.up }) && g.At(x, y+1).Node == "" {
				bad("down")
			}
			if d.right && !joins(point{x + 1, y}, func(d dirs) bool { return d.left }) && g.At(x+1, y).Node == "" {
				bad("right")
			}
			// Lines leave a horizontal node after one space.
			if d.left && !joins(point{x - 1, y}, func(d dirs) bool { return d.right }) && g.At(x-2, y).Node == "" {
				bad("left")
			}
		}
	}
	return out
}

// falseEdgeKind separates a trunk that joins sources sharing only some of
// their targets from a trunk that joins edges with nothing in common.
func falseEdgeKind(g layout.Graph, from, to string) string {
	targets := func(id string) []string {
		var out []string
		for _, n := range g.Nodes {
			if slices.Contains(n.Needs, id) {
				out = append(out, n.ID)
			}
		}
		return out
	}
	mine := targets(from)
	for _, n := range g.Nodes {
		if n.ID != to {
			continue
		}
		for _, need := range n.Needs {
			if slices.ContainsFunc(targets(need), func(t string) bool { return slices.Contains(mine, t) }) {
				return "false-edge-partial-overlap"
			}
		}
	}
	return "false-edge-unrelated"
}

// wastedSpace flags the kind of gap the isolated-node bug produced.
func wastedSpace(gr layout.Graph, g *layout.Grid) []violation {
	var out []violation
	lines := g.Lines()
	for y := 1; y+1 < len(lines); y++ {
		if lines[y] == "" {
			out = append(out, violation{"blank-row", fmt.Sprintf("row %d", y)})
		}
	}
	run := 0
	for x := 0; x < g.Width; x++ {
		blank := true
		for y := range g.Height {
			if g.At(x, y).Text != " " {
				blank = false
				break
			}
		}
		if blank {
			run++
		} else {
			run = 0
		}
		if run == 3 {
			out = append(out, violation{"blank-columns", fmt.Sprintf("three blank columns end at %d", x)})
		}
	}
	if g.Orientation != layout.Horizontal {
		return out
	}

	group := components(gr)
	children := map[string][]string{}
	parents := map[string][]string{}
	for _, n := range gr.Nodes {
		for _, need := range n.Needs {
			children[need] = append(children[need], n.ID)
			parents[n.ID] = append(parents[n.ID], need)
		}
	}
	for _, lv := range g.Levels {
		for i := 0; i+1 < len(lv); i++ {
			a, b := g.Boxes[lv[i]], g.Boxes[lv[i+1]]
			if group[lv[i]] == group[lv[i+1]] && b.Y-a.Y > 2 {
				out = append(out, violation{"node-gap", fmt.Sprintf("%s and %s are %d rows apart", lv[i], lv[i+1], b.Y-a.Y)})
			}
		}
	}
	// A fan-out should sit around its parent, not hang off one side.
	for id, kids := range children {
		if len(kids) < 2 {
			continue
		}
		lo, hi := g.Height, 0
		for _, k := range kids {
			lo, hi = min(lo, g.Boxes[k].Y), max(hi, g.Boxes[k].Y)
		}
		if y := g.Boxes[id].Y; y < lo-1 || y > hi+1 {
			out = append(out, violation{"parent-outside-fan", fmt.Sprintf("%s at row %d, children in %d..%d", id, y, lo, hi)})
		}
	}
	// A single child far from its only parent reads as a long detour.
	for id, ps := range parents {
		if len(ps) == 1 && len(children[ps[0]]) == 1 {
			if d := g.Boxes[id].Y - g.Boxes[ps[0]].Y; d < -1 || d > 1 {
				out = append(out, violation{"chain-kinked", fmt.Sprintf("%s -> %s jumps %d rows", ps[0], id, d)})
			}
		}
	}
	return out
}

func components(g layout.Graph) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		parent[x] = x
		return x
	}
	for _, n := range g.Nodes {
		find(n.ID)
		for _, need := range n.Needs {
			parent[find(need)] = find(n.ID)
		}
	}
	out := map[string]string{}
	for _, n := range g.Nodes {
		out[n.ID] = find(n.ID)
	}
	return out
}

// ciLike wires a root, a fan-out, a fan-in, and a tail.
func ciLike(r *rand.Rand, needs [][]int) {
	n := len(needs)
	for i := 1; i < n; i++ {
		switch {
		case i <= n/2:
			needs[i] = []int{0}
		case i == n/2+1:
			for j := 1; j <= n/2; j++ {
				needs[i] = append(needs[i], j)
			}
		default:
			needs[i] = []int{i - 1}
			if r.Float64() < 0.4 {
				needs[i] = append(needs[i], 1+r.IntN(n/2))
			}
		}
	}
}

// separateParts wires several components, which can leave isolated nodes.
func separateParts(r *rand.Rand, needs [][]int) {
	n := len(needs)
	parts := 2 + r.IntN(3)
	for i := parts; i < n; i++ {
		lo := i % parts
		needs[i] = []int{lo}
		for j := parts; j < i; j++ {
			if j%parts == lo && r.Float64() < 0.4 {
				needs[i] = append(needs[i], j)
			}
		}
	}
}

// generate draws a DAG of one of several shapes. IDs are declared in a
// shuffled order for some graphs, since barycenter ties follow declaration.
func generate(r *rand.Rand) layout.Graph {
	n := 2 + r.IntN(10)
	needs := make([][]int, n)
	switch r.IntN(5) {
	case 0: // uniform density
		p := []float64{0.1, 0.2, 0.35, 0.6}[r.IntN(4)]
		for i := range n {
			for j := range i {
				if r.Float64() < p {
					needs[i] = append(needs[i], j)
				}
			}
		}
	case 1: // layered: edges mostly between neighboring layers
		layers := 2 + r.IntN(4)
		layer := make([]int, n)
		for i := range n {
			layer[i] = i * layers / n
		}
		for i := range n {
			for j := range i {
				if layer[i] == layer[j]+1 && r.Float64() < 0.5 || layer[i] > layer[j]+1 && r.Float64() < 0.1 {
					needs[i] = append(needs[i], j)
				}
			}
		}
	case 2: // chain with skips
		for i := 1; i < n; i++ {
			needs[i] = append(needs[i], i-1)
			for j := range i - 1 {
				if r.Float64() < 0.2 {
					needs[i] = append(needs[i], j)
				}
			}
		}
	case 3:
		ciLike(r, needs)
	case 4:
		separateParts(r, needs)
	}

	order := r.Perm(n)
	if r.IntN(2) == 0 {
		for i := range order {
			order[i] = i
		}
	}
	name := func(i int) string { return string(rune('a' + i)) }
	nodes := make([]layout.Node, 0, n)
	for _, i := range order {
		nd := layout.Node{ID: name(i), Needs: nil}
		for _, j := range needs[i] {
			nd.Needs = append(nd.Needs, name(j))
		}
		nodes = append(nodes, nd)
	}
	return layout.Graph{Nodes: nodes}
}

func size(g layout.Graph) int {
	s := len(g.Nodes)
	for _, n := range g.Nodes {
		s += len(n.Needs)
	}
	return s
}

func cloneGraph(g layout.Graph) layout.Graph {
	nodes := make([]layout.Node, len(g.Nodes))
	for i, n := range g.Nodes {
		nodes[i] = layout.Node{ID: n.ID, Needs: slices.Clone(n.Needs)}
	}
	return layout.Graph{Nodes: nodes}
}

func hasCategory(g layout.Graph, sc scenario, cat string) bool {
	return slices.ContainsFunc(check(g, sc), func(v violation) bool { return v.category == cat })
}

// shrink removes nodes and edges while the category still fires.
func shrink(g layout.Graph, sc scenario, cat string) layout.Graph {
	for changed := true; changed; {
		changed = false
		for i := range g.Nodes {
			c := cloneGraph(g)
			gone := c.Nodes[i].ID
			c.Nodes = slices.Delete(c.Nodes, i, i+1)
			for k := range c.Nodes {
				c.Nodes[k].Needs = slices.DeleteFunc(c.Nodes[k].Needs, func(s string) bool { return s == gone })
			}
			if hasCategory(c, sc, cat) {
				g, changed = c, true
				break
			}
		}
		if changed {
			continue
		}
	edges:
		for i := range g.Nodes {
			for j := range g.Nodes[i].Needs {
				c := cloneGraph(g)
				c.Nodes[i].Needs = slices.Delete(c.Nodes[i].Needs, j, j+1)
				if hasCategory(c, sc, cat) {
					g, changed = c, true
					break edges
				}
			}
		}
	}
	return g
}

func describe(g layout.Graph) string {
	parts := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if len(n.Needs) == 0 {
			parts = append(parts, n.ID)
			continue
		}
		parts = append(parts, n.ID+"<-"+strings.Join(n.Needs, ","))
	}
	return strings.Join(parts, "  ")
}

// TestRandomGraphInvariants checks what must hold for every graph. False edges
// and spacing are absent because the layout does not meet them yet; run the
// report to see those.
func TestRandomGraphInvariants(t *testing.T) {
	hard := []string{
		"nondeterministic", "missing-node", "node-overwritten", "edge-through-node", "node-text-damaged",
		"missing-edge", "highlight-broken", "untagged-line", "arrow-tag-wrong", "arrow-to-nothing",
		"dangling-line", "blank-row",
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 500 {
		g := generate(r)
		for _, o := range []layout.Orientation{layout.Horizontal, layout.Vertical} {
			for m := range cellModeCount {
				sc := scenario{o, m}
				for _, v := range check(g, sc) {
					if slices.Contains(hard, v.category) {
						small := shrink(g, sc, v.category)
						t.Fatalf("%s (%s/%s): %s\ngraph: %s\n%s", v.category, o, m, v.detail, describe(small), render(small, sc))
					}
				}
			}
		}
	}
}

// TestRandomGraphsReport writes a report of every category it finds. It runs
// only when WEFT_FUZZ_REPORT names the output file.
func TestRandomGraphsReport(t *testing.T) {
	path := os.Getenv("WEFT_FUZZ_REPORT")
	if path == "" {
		t.Skip("set WEFT_FUZZ_REPORT to write a report")
	}
	const graphs = 5000

	type finding struct {
		count   int
		graphs  int
		scs     map[string]int
		smallG  layout.Graph
		smallSc scenario
		have    bool
	}
	findings := map[string]*finding{}
	r := rand.New(rand.NewPCG(1, 2))
	scenarios := []scenario{}
	for _, o := range []layout.Orientation{layout.Horizontal, layout.Vertical} {
		for m := range cellModeCount {
			scenarios = append(scenarios, scenario{o, m})
		}
	}
	anyFalse := map[string][2]int{}
	for range graphs {
		g := generate(r)
		bucket := "over 6 nodes"
		if len(g.Nodes) <= 6 {
			bucket = "up to 6 nodes"
		}
		for _, sc := range scenarios {
			seen := map[string]bool{}
			counts := anyFalse[bucket]
			counts[1]++
			if slices.ContainsFunc(check(g, sc), func(v violation) bool { return strings.HasPrefix(v.category, "false-edge") }) {
				counts[0]++
			}
			anyFalse[bucket] = counts
			for _, v := range check(g, sc) {
				f := findings[v.category]
				if f == nil {
					f = &finding{scs: map[string]int{}}
					findings[v.category] = f
				}
				f.count++
				if !seen[v.category] {
					seen[v.category] = true
					f.graphs++
					f.scs[fmt.Sprintf("%s/%s", sc.orient, sc.cells)]++
					if !f.have || size(g) < size(f.smallG) {
						f.smallG, f.smallSc, f.have = g, sc, true
					}
				}
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d graphs x %d scenarios = %d layouts\n", graphs, len(scenarios), graphs*len(scenarios))
	for _, k := range []string{"up to 6 nodes", "over 6 nodes"} {
		fmt.Fprintf(&b, "any false edge, %s: %d of %d\n", k, anyFalse[k][0], anyFalse[k][1])
	}
	cats := make([]string, 0, len(findings))
	for c := range findings {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		f := findings[c]
		fmt.Fprintf(&b, "\n=== %s: %d layouts, %d hits\n", c, f.graphs, f.count)
		keys := make([]string, 0, len(f.scs))
		for k := range f.scs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %-24s %d\n", k, f.scs[k])
		}
		small := shrink(f.smallG, f.smallSc, c)
		fmt.Fprintf(&b, "smallest (%s/%s): %s\n", f.smallSc.orient, f.smallSc.cells, describe(small))
		fmt.Fprintln(&b, render(small, f.smallSc).String())
		for _, v := range check(small, f.smallSc) {
			if v.category == c {
				fmt.Fprintf(&b, "  -> %s\n", v.detail)
			}
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRandomGraphsSnapshots dumps larger graphs for reading by eye. It runs
// only when WEFT_FUZZ_SNAPSHOTS names the output file.
func TestRandomGraphsSnapshots(t *testing.T) {
	path := os.Getenv("WEFT_FUZZ_SNAPSHOTS")
	if path == "" {
		t.Skip("set WEFT_FUZZ_SNAPSHOTS to write snapshots")
	}
	r := rand.New(rand.NewPCG(7, 11))
	var b strings.Builder
	for i := 0; i < 24; {
		g := generate(r)
		if len(g.Nodes) < 7 {
			continue
		}
		i++
		fmt.Fprintf(&b, "--- #%d %s\n", i, describe(g))
		fmt.Fprintln(&b, render(g, scenario{layout.Horizontal, cellPlain}).String())
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, render(g, scenario{layout.Vertical, cellPlain}).String())
		fmt.Fprintln(&b)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
