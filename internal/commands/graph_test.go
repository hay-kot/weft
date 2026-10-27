package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/workflow"
)

func TestGraphJSON(t *testing.T) {
	wf, err := workflow.Parse([]byte("nodes:\n  a: {run: x, label: Alpha}\n  b: {run: y, needs: [a]}\n"))
	require.NoError(t, err)
	wf.Name = "demo"
	grid := layout.Layout(layout.FromWorkflow(wf), layout.Options{
		Width: 0, Orientation: layout.Auto, Cell: nil, MaxCellWidth: 0,
	})

	out := graphJSON(wf, grid)
	assert.Equal(t, "demo", out.Workflow)
	assert.Equal(t, "horizontal", out.Orientation)
	assert.Equal(t, [][]string{{"a"}, {"b"}}, out.Levels)
	assert.Equal(t, []graphNode{
		{ID: "a", Label: "Alpha", Level: 0, Needs: []string{}},
		{ID: "b", Label: "b", Level: 1, Needs: []string{"a"}},
	}, out.Nodes)
	assert.Equal(t, []string{"a ───▸ b"}, out.Lines)
}
