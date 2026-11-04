package layout_test

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/workflow"
)

var update = flag.Bool("update", false, "rewrite golden files")

func node(id string, needs ...string) layout.Node {
	return layout.Node{ID: id, Needs: needs}
}

func graph(nodes ...layout.Node) layout.Graph {
	return layout.Graph{Nodes: nodes}
}

var deploy = graph(
	node("guard"),
	node("go-check", "guard"),
	node("ui-check", "guard"),
	node("db-backup", "guard"),
	node("release", "go-check", "ui-check", "db-backup"),
	node("deploy", "release"),
	node("tofu-apply", "deploy"),
)

var isolatedNode = graph(
	node("gen"),
	node("validate"),
	node("go-check", "gen"),
	node("ui-check", "gen"),
	node("integration", "go-check"),
	node("e2e", "integration", "ui-check"),
)

var sharedSkip = graph(
	node("start"),
	node("go-check", "start"),
	node("ui-check", "start"),
	node("e2e", "go-check", "ui-check"),
	node("backup", "start"),
	node("release", "e2e", "backup"),
	node("apply", "e2e", "backup"),
)

var lineCrossesConnector = graph(
	node("a"), node("b", "a"), node("c", "b"), node("d", "c", "a"),
	node("e", "d"), node("f", "e", "b"), node("g", "f", "a", "b"), node("h", "g"),
)

// tuiCell mimics the TUI's cells: an icon, the label, and a duration.
func tuiCell(id string) layout.Cell {
	icons := map[string]string{"guard": "✓", "go-check": "✓", "ui-check": "✓", "db-backup": "✓", "release": "●", "tofu-apply": "◆"}
	durations := map[string]string{"guard": "0:00", "go-check": "2:10", "ui-check": "1:48", "db-backup": "0:22", "release": "1:31"}
	icon := icons[id]
	if icon == "" {
		icon = "○"
	}
	suffix := ""
	if d := durations[id]; d != "" {
		suffix = " " + d
	}
	return layout.Cell{Prefix: icon + " ", Label: id, Suffix: suffix}
}

func opts(o layout.Orientation, width int) layout.Options {
	return layout.Options{Width: width, Orientation: o, Cell: nil, MaxCellWidth: 0}
}

func TestGolden(t *testing.T) {
	tests := []struct {
		name  string
		graph layout.Graph
		opts  layout.Options
	}{
		{"deploy", deploy, opts(layout.Auto, 0)},
		{"deploy-tui-cells", deploy, layout.Options{Width: 0, Orientation: layout.Horizontal, Cell: tuiCell, MaxCellWidth: 0}},
		{"deploy-vertical", deploy, opts(layout.Vertical, 0)},
		{"deploy-narrow-wraps", deploy, opts(layout.Auto, 40)},
		{"deploy-too-narrow-to-wrap-falls-back-to-vertical", deploy, opts(layout.Auto, 30)},
		{"deploy-horizontal-wraps-past-width", deploy, opts(layout.Horizontal, 30)},
		{"deploy-tui-cells-wrapped", deploy, layout.Options{Width: 60, Orientation: layout.Horizontal, Cell: tuiCell, MaxCellWidth: 0}},
		{"fan-out", graph(node("root"), node("a", "root"), node("b", "root"), node("c", "root"), node("d", "root"), node("e", "root")), opts(layout.Auto, 0)},
		{"fan-in", graph(node("a"), node("b"), node("c"), node("d"), node("sink", "a", "b", "c", "d")), opts(layout.Auto, 0)},
		{"fan-in-vertical", graph(node("a"), node("b"), node("c"), node("d"), node("sink", "a", "b", "c", "d")), opts(layout.Vertical, 0)},
		{"level-skip", graph(node("a"), node("b", "a"), node("c", "b"), node("d", "a", "c")), opts(layout.Auto, 0)},
		{"level-skip-vertical", graph(node("a"), node("b", "a"), node("c", "b"), node("d", "a", "c")), opts(layout.Vertical, 0)},
		{"long-skip-past-fan", graph(
			node("start"), node("x", "start"), node("y", "start"),
			node("x2", "x"), node("y2", "y"), node("end", "x2", "y2", "start"),
		), opts(layout.Auto, 0)},
		{"diamond", graph(node("top"), node("left", "top"), node("right", "top"), node("bottom", "left", "right")), opts(layout.Auto, 0)},
		{"two-components", graph(node("a1"), node("a2", "a1"), node("b1"), node("b2", "b1"), node("b3", "b2")), opts(layout.Auto, 0)},
		{"isolated-node", isolatedNode, opts(layout.Auto, 0)},
		{"isolated-node-vertical", isolatedNode, opts(layout.Vertical, 0)},
		{"shared-skip", sharedSkip, opts(layout.Horizontal, 0)},
		{"shared-skip-vertical", sharedSkip, opts(layout.Vertical, 0)},
		{"line-crosses-connector", lineCrossesConnector, opts(layout.Horizontal, 0)},
		{"line-crosses-connector-vertical", lineCrossesConnector, opts(layout.Vertical, 0)},
		{"crossing", graph(node("p"), node("q"), node("r", "q"), node("s", "p")), opts(layout.Auto, 0)},
		{"single", graph(node("only")), opts(layout.Auto, 0)},
		{"truncated-labels", graph(
			node("a-very-long-node-name-indeed"), node("short", "a-very-long-node-name-indeed"),
		), opts(layout.Auto, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := layout.Layout(tt.graph, tt.opts).String() + "\n"
			path := filepath.Join("testdata", tt.name+".golden")
			if *update {
				require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "run go test ./internal/layout -update to create golden files")
			assert.Equal(t, string(want), got)
		})
	}
}

func TestDeterministic(t *testing.T) {
	first := layout.Layout(deploy, opts(layout.Auto, 0)).String()
	for range 20 {
		assert.Equal(t, first, layout.Layout(deploy, opts(layout.Auto, 0)).String())
	}
}

func TestEmptyGraph(t *testing.T) {
	g := layout.Layout(graph(), opts(layout.Auto, 0))
	assert.Empty(t, g.String())
	assert.Equal(t, 0, g.Height)
}

func TestAutoOrientation(t *testing.T) {
	wide := layout.Layout(deploy, opts(layout.Auto, 0))
	assert.Equal(t, layout.Horizontal, wide.Orientation)
	fits := layout.Layout(deploy, opts(layout.Auto, wide.Width))
	assert.Equal(t, layout.Horizontal, fits.Orientation)
	wrapped := layout.Layout(deploy, opts(layout.Auto, wide.Width-1))
	assert.Equal(t, layout.Horizontal, wrapped.Orientation)
	assert.NotNil(t, wrapped.Bands, "a narrower width wraps into bands")
	assert.Nil(t, fits.Bands)
	narrow := layout.Layout(deploy, opts(layout.Auto, 30))
	assert.Equal(t, layout.Vertical, narrow.Orientation, "the fan-out band alone is wider than 30")
}

const nRelease = "release"

func TestWrappedBands(t *testing.T) {
	g := layout.Layout(deploy, opts(layout.Auto, 40))
	require.Equal(t, []int{0, 4}, g.Bands, "\n%s", g.String())
	assert.Equal(t, [][]string{
		{"guard"}, {"go-check", "ui-check", "db-backup"}, {nRelease}, {"deploy"}, {"tofu-apply"},
	}, g.Levels)

	box := g.Boxes[nRelease]
	assert.Equal(t, 2, box.Level)
	assert.Equal(t, 1, box.Y)
	copied, ok := g.Copies[nRelease]
	require.True(t, ok)
	assert.Equal(t, layout.Box{ID: nRelease, Level: 2, Index: 0, X: 1, Y: 4, Width: box.Width}, copied)
	assert.Len(t, g.Copies, 1)

	var text strings.Builder
	for x := copied.X - 1; x <= copied.X+copied.Width; x++ {
		assert.Equal(t, nRelease, g.At(x, copied.Y).Node)
		text.WriteString(g.At(x, copied.Y).Text)
	}
	assert.Equal(t, "[release]", text.String())
	assert.Equal(t, "deploy", g.Boxes["deploy"].ID)
	assert.Equal(t, 4, g.Boxes["deploy"].Y)

	toDeploy := slices.Index(g.Edges, layout.Edge{From: nRelease, To: "deploy"})
	require.GreaterOrEqual(t, toDeploy, 0)
	exit := g.At(copied.X+copied.Width+2, copied.Y)
	assert.Equal(t, "─", exit.Text)
	assert.Equal(t, []int{toDeploy}, exit.Edges, "the line out of the copy carries release -> deploy")
}

func TestWrapNeedsACut(t *testing.T) {
	g := layout.Layout(graph(node("a"), node("b"), node("c", "a", "b"), node("d", "a", "b")), opts(layout.Auto, 5))
	assert.Equal(t, layout.Vertical, g.Orientation)
	assert.Nil(t, g.Bands)
}

func TestLevelsAndBoxes(t *testing.T) {
	g := layout.Layout(deploy, opts(layout.Horizontal, 0))
	assert.Equal(t, [][]string{
		{"guard"}, {"go-check", "ui-check", "db-backup"}, {"release"}, {"deploy"}, {"tofu-apply"},
	}, g.Levels)

	box := g.Boxes["ui-check"]
	assert.Equal(t, 1, box.Level)
	assert.Equal(t, 1, box.Index)
	assert.Equal(t, len("ui-check"), box.Width)
	var text strings.Builder
	for x := box.X; x < box.X+box.Width; x++ {
		assert.Equal(t, "ui-check", g.At(x, box.Y).Node)
		text.WriteString(g.At(x, box.Y).Text)
	}
	assert.Equal(t, "ui-check", text.String())
	assert.Empty(t, g.At(box.X-1, box.Y).Node)
}

func TestGlyphsCarryTheirEdges(t *testing.T) {
	g := layout.Layout(graph(node("a"), node("b", "a"), node("c", "a")), opts(layout.Horizontal, 0))
	require.Equal(t, []layout.Edge{{From: "a", To: "b"}, {From: "a", To: "c"}}, g.Edges)

	toC := 1
	found := false
	for y := range g.Height {
		for x := range g.Width {
			gl := g.At(x, y)
			if gl.Text == "▸" && y == g.Boxes["c"].Y {
				assert.Contains(t, gl.Edges, toC)
				assert.NotContains(t, gl.Edges, 0, "the arrow into c belongs only to a->c")
				found = true
			}
		}
	}
	assert.True(t, found, "no arrowhead into c:\n%s", g.String())
	assert.ElementsMatch(t, []int{0, 1}, g.EdgesOf("a"))
	assert.Equal(t, []int{1}, g.EdgesOf("c"))
	assert.Equal(t, " ", g.At(-1, 0).Text)
}

func TestSharedSkipCarriesEveryEdge(t *testing.T) {
	g := layout.Layout(graph(node("a"), node("b", "a"), node("c", "a", "b"), node("d", "a", "b")), opts(layout.Horizontal, 0))
	require.Equal(t, layout.Edge{From: "a", To: "c"}, g.Edges[1])
	require.Equal(t, layout.Edge{From: "a", To: "d"}, g.Edges[3])

	b := g.Boxes["b"]
	passes := 0
	for y := range g.Height {
		if gl := g.At(b.X, y); y != b.Y && gl.Text == "─" {
			assert.ElementsMatch(t, []int{1, 3}, gl.Edges)
			passes++
		}
	}
	assert.Equal(t, 1, passes, "a's skip edges should share one row:\n%s", g.String())
}

func TestWideLabels(t *testing.T) {
	g := layout.Layout(graph(node("a"), node("b", "a")), layout.Options{
		Width: 0, Orientation: layout.Horizontal, MaxCellWidth: 0,
		Cell: func(id string) layout.Cell {
			return layout.Cell{Prefix: "", Label: map[string]string{"a": "部署", "b": "b"}[id], Suffix: ""}
		},
	})
	assert.Equal(t, "部署 ───▸ b", g.String())
	assert.Equal(t, 4, g.Boxes["a"].Width)
}

func TestGraphemeClusterLabels(t *testing.T) {
	family := "👨\u200d👩\u200d👧 family"
	g := layout.Layout(graph(node("a"), node("b", "a")), layout.Options{
		Width: 0, Orientation: layout.Horizontal, MaxCellWidth: 0,
		Cell: func(id string) layout.Cell {
			return layout.Cell{Prefix: "", Label: map[string]string{"a": family, "b": family}[id], Suffix: ""}
		},
	})
	assert.Equal(t, family+" ───▸ "+family, g.String())
	assert.Equal(t, 9, g.Boxes["b"].Width)
	assert.Equal(t, "b", g.At(g.Width-1, 0).Node)
}

func TestFromWorkflow(t *testing.T) {
	wf, err := workflow.Parse([]byte("nodes:\n  a: {run: x}\n  b: {run: y, needs: [a]}\n"))
	require.NoError(t, err)
	g := layout.FromWorkflow(wf)
	assert.Equal(t, graph(node("a"), node("b", "a")), g)
	assert.Equal(t, "a ───▸ b", strings.TrimSpace(layout.Layout(g, opts(layout.Auto, 0)).String()))
}
