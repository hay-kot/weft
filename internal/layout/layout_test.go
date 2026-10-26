package layout_test

import (
	"flag"
	"os"
	"path/filepath"
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
		{"deploy-narrow-falls-back-to-vertical", deploy, opts(layout.Auto, 40)},
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
	narrow := layout.Layout(deploy, opts(layout.Auto, wide.Width-1))
	assert.Equal(t, layout.Vertical, narrow.Orientation)
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
