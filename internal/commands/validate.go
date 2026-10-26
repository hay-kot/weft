package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/workflow"
)

// ValidateCmd implements the validate command.
type ValidateCmd struct {
	flags *Flags
	json  bool
}

// NewValidateCmd creates a new validate command.
func NewValidateCmd(flags *Flags) *ValidateCmd {
	return &ValidateCmd{flags: flags, json: false}
}

// Register adds the validate command to the application.
func (cmd *ValidateCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:      "validate",
		Usage:     "check workflow files for errors",
		ArgsUsage: "[workflow]",
		Description: `Parses a workflow and checks node IDs, fields, needs, and cycles. With no
argument, checks every workflow in the nearest .weft/ directory. Exits
non-zero when any workflow is invalid.

With --json, prints an array with one object per workflow: workflow, path,
valid, nodes (the node count), and problems (one message per problem).

Examples:
  weft validate
  weft validate deploy --json`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "json",
				Usage:       "print one result per workflow as JSON",
				Destination: &cmd.json,
			},
		},
		Action: cmd.run,
	})

	return app
}

type validateResult struct {
	Workflow string   `json:"workflow"`
	Path     string   `json:"path"`
	Valid    bool     `json:"valid"`
	Nodes    int      `json:"nodes"`
	Problems []string `json:"problems,omitempty"`
}

func (cmd *ValidateCmd) run(_ context.Context, c *cli.Command) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var targets [][2]string // path, root
	switch c.NArg() {
	case 0:
		root, err := workflow.FindRoot(cwd)
		if err != nil {
			return err
		}
		names, err := workflow.List(root)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("no workflow files in %s", filepath.Join(root, workflow.DirName))
		}
		for _, name := range names {
			path, _, err := workflow.Resolve(cwd, name)
			if err != nil {
				return err
			}
			targets = append(targets, [2]string{path, root})
		}
	case 1:
		path, root, err := workflow.Resolve(cwd, c.Args().First())
		if err != nil {
			return err
		}
		targets = append(targets, [2]string{path, root})
	default:
		return errors.New("usage: weft validate [workflow]")
	}

	results := make([]validateResult, 0, len(targets))
	invalid := 0
	for _, t := range targets {
		res := validateResult{Workflow: "", Path: t[0], Valid: true, Nodes: 0, Problems: nil}
		wf, err := workflow.Load(t[0], t[1])
		var verr *workflow.ValidationError
		switch {
		case errors.As(err, &verr):
			res.Valid = false
			res.Problems = verr.Problems
		case err != nil:
			res.Valid = false
			res.Problems = []string{err.Error()}
		default:
			res.Workflow = wf.Name
			res.Nodes = len(wf.Nodes)
		}
		if !res.Valid {
			invalid++
		}
		results = append(results, res)
	}

	if cmd.json {
		if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			rel := r.Path
			if p, err := filepath.Rel(cwd, r.Path); err == nil {
				rel = p
			}
			if r.Valid {
				noun := "nodes"
				if r.Nodes == 1 {
					noun = "node"
				}
				fmt.Printf("ok    %s (%d %s)\n", rel, r.Nodes, noun)
				continue
			}
			fmt.Printf("error %s\n", rel)
			for _, p := range r.Problems {
				fmt.Printf("      - %s\n", p)
			}
		}
	}

	if invalid > 0 {
		return fmt.Errorf("%d of %d workflows invalid", invalid, len(results))
	}
	return nil
}
