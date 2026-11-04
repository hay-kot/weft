package layout

// A wrapped layout splits the horizontal layout into bands stacked top to
// bottom. A band ends at a cut: a level that holds one node and that no
// edge skips over, so every edge out of the band leaves that node. The next
// band starts with a copy of the node in brackets, and its edges leave the
// copy. Each band is laid out on its own, as the horizontal layout of the
// subgraph between its cuts.

// wrap packs the levels into bands no wider than opts.Width where it can:
// each band runs to the furthest cut that still fits, or to the nearest
// cut when none does. It returns nil when the graph has no cut.
func wrap(g Graph, opts Options, full *builder) *Grid {
	last := len(full.levels) - 1
	var ends []int
	for l := range last {
		if len(full.levels[l]) == 1 && full.levels[l][0].real {
			ends = append(ends, l)
		}
	}
	if len(ends) == 0 {
		return nil
	}
	ends = append(ends, last)

	level := map[string]int{}
	for l, lv := range full.levels {
		for _, it := range lv {
			if it.real {
				level[it.id] = l
			}
		}
	}

	type band struct {
		start int
		grid  *Grid
	}
	var bands []band
	for start := 0; start < last; {
		var best *Grid
		bestEnd := 0
		for _, end := range ends {
			if end <= start {
				continue
			}
			bg := renderBand(g, opts, level, start, end)
			if best != nil && bg.Width > opts.Width {
				break
			}
			best, bestEnd = bg, end
		}
		bands = append(bands, band{start: start, grid: best})
		start = bestEnd
	}

	height := len(bands) - 1
	width := 0
	for _, bd := range bands {
		height += bd.grid.Height
		width = max(width, bd.grid.Width)
	}
	c := newCanvas(width, height)
	out := full.grid(c, Horizontal)
	edgeIndex := map[Edge]int{}
	for i, e := range out.Edges {
		edgeIndex[e] = i
	}

	y0 := 0
	for k, bd := range bands {
		out.Bands = append(out.Bands, y0)
		bg := bd.grid
		for y := range bg.Height {
			for x := range bg.Width {
				gl := bg.At(x, y)
				edges := make([]int, 0, len(gl.Edges))
				for _, e := range gl.Edges {
					edges = append(edges, edgeIndex[bg.Edges[e]])
				}
				c.glyphs[y0+y][x] = Glyph{Text: gl.Text, Node: gl.Node, Edges: edges}
			}
		}
		for l, ids := range bg.Levels {
			if k == 0 || l > 0 {
				out.Levels[bd.start+l] = ids
			}
		}
		for id, b := range bg.Boxes {
			b.Level += bd.start
			b.Y += y0
			if k > 0 && b.Level == bd.start {
				b.X++
				b.Width -= 2
				out.Copies[id] = b
				continue
			}
			out.Boxes[id] = b
		}
		y0 += bg.Height + 1
	}
	return out
}

// renderBand lays out the levels from start to end. Unless the band starts
// the graph, the node at start is the copy of the previous band's last node.
func renderBand(g Graph, opts Options, level map[string]int, start, end int) *Grid {
	in := func(id string) bool {
		l, ok := level[id]
		return ok && l >= start && l <= end
	}
	var sub Graph
	for _, n := range g.Nodes {
		if !in(n.ID) {
			continue
		}
		nd := Node{ID: n.ID, Needs: nil}
		for _, need := range n.Needs {
			if in(need) {
				nd.Needs = append(nd.Needs, need)
			}
		}
		sub.Nodes = append(sub.Nodes, nd)
	}

	b := prepare(sub, opts)
	if start > 0 {
		it := b.levels[0][0]
		it.text = "[" + it.text + "]"
		it.width += 2
	}
	return b.renderHorizontal()
}
