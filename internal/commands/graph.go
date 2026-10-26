package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/term"
	"github.com/hay-kot/weft/internal/workflow"
)

// Layout names for --layout.
const (
	layoutAuto       = "auto"
	layoutHorizontal = "horizontal"
	layoutVertical   = "vertical"
)

// GraphCmd implements the graph command.
type GraphCmd struct {
	flags  *Flags
	layout string
	width  int
	json   bool
}

// NewGraphCmd creates a new graph command.
func NewGraphCmd(flags *Flags) *GraphCmd {
	return &GraphCmd{flags: flags, layout: layoutAuto, width: -1, json: false}
}

// Register adds the graph command to the application.
func (cmd *GraphCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "graph",
		Usage:     "print a workflow's graph",
		ArgsUsage: "<workflow>",
		Description: `Prints the workflow's nodes in levels, with edges drawn between them: the
same layout the TUI shows during a run. Levels run left to right; when that
is wider than the terminal, they run top to bottom instead.

With --json, prints the nodes with their levels and needs, plus the drawn
lines.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "layout",
				Usage:       "auto, horizontal, or vertical",
				Value:       layoutAuto,
				Destination: &cmd.layout,
				Validator: func(s string) error {
					switch s {
					case layoutAuto, layoutHorizontal, layoutVertical:
						return nil
					default:
						return fmt.Errorf("--layout must be auto, horizontal, or vertical, got %q", s)
					}
				},
			},
			&cli.IntFlag{
				Name:        "width",
				Usage:       "width available for the auto layout (default: the terminal width; unlimited when not a terminal)",
				Value:       -1,
				Destination: &cmd.width,
			},
			&cli.BoolFlag{
				Name:        "json",
				Usage:       "print nodes, levels, and the drawn lines as JSON",
				Destination: &cmd.json,
			},
		},
		Action: cmd.run,
	})

	return app
}

type graphNode struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Level int      `json:"level"`
	Needs []string `json:"needs"`
}

type graphOutput struct {
	Workflow    string      `json:"workflow"`
	Orientation string      `json:"orientation"`
	Levels      [][]string  `json:"levels"`
	Nodes       []graphNode `json:"nodes"`
	Lines       []string    `json:"lines"`
}

func (cmd *GraphCmd) run(_ context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return errors.New("usage: weft graph <workflow>")
	}
	wf, err := loadWorkflow(c.Args().First())
	if err != nil {
		return err
	}

	width := cmd.width
	if width < 0 {
		width = term.Width(os.Stdout)
	}
	orient := map[string]layout.Orientation{
		layoutAuto:       layout.Auto,
		layoutHorizontal: layout.Horizontal,
		layoutVertical:   layout.Vertical,
	}[cmd.layout]

	grid := layout.Layout(layout.FromWorkflow(wf), layout.Options{
		Width:       width,
		Orientation: orient,
		Cell: func(id string) layout.Cell {
			return layout.Cell{Prefix: "", Label: wf.Node(id).DisplayName(), Suffix: ""}
		},
		MaxCellWidth: 0,
	})

	if cmd.json {
		return json.NewEncoder(os.Stdout).Encode(graphJSON(wf, grid))
	}
	if len(wf.Nodes) == 0 {
		return nil
	}
	_, err = fmt.Println(strings.Join(grid.Lines(), "\n"))
	return err
}

func graphJSON(wf *workflow.Workflow, grid *layout.Grid) graphOutput {
	out := graphOutput{
		Workflow:    wf.Name,
		Orientation: grid.Orientation.String(),
		Levels:      grid.Levels,
		Nodes:       make([]graphNode, 0, len(wf.Nodes)),
		Lines:       grid.Lines(),
	}
	for _, n := range wf.Nodes {
		needs := n.Needs
		if needs == nil {
			needs = []string{}
		}
		out.Nodes = append(out.Nodes, graphNode{
			ID: n.ID, Label: n.DisplayName(), Level: grid.Boxes[n.ID].Level, Needs: needs,
		})
	}
	return out
}
