package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/workflow"
)

// completeWorkflow offers the workflow names in the nearest .weft/ for a
// command's <workflow> argument. The shell narrows them to what was typed.
// A word that starts with a dash is a flag, which urfave completes.
func completeWorkflow(ctx context.Context, c *cli.Command) {
	if completingFlag(c) {
		cli.DefaultCompleteWithFlags(ctx, c)
		return
	}
	switch c.NArg() {
	case 0:
		printWorkflows(c)
	case 1:
		if _, err := loadWorkflow(c.Args().First()); err != nil {
			// A name typed in part is not a workflow yet.
			printWorkflows(c)
		}
	}
}

// completeWorkflowThenNode offers workflow names for the first argument and
// the workflow's node IDs for the second.
func completeWorkflowThenNode(ctx context.Context, c *cli.Command) {
	if completingFlag(c) {
		cli.DefaultCompleteWithFlags(ctx, c)
		return
	}
	switch c.NArg() {
	case 0:
		printWorkflows(c)
	case 1:
		wf, err := loadWorkflow(c.Args().First())
		if err != nil {
			printWorkflows(c)
			return
		}
		for _, n := range wf.Nodes {
			_, _ = fmt.Fprintln(c.Root().Writer, n.ID)
		}
	}
}

// completingFlag reports whether the word being completed is a flag. In
// completion mode urfave leaves the partial flag among the arguments.
func completingFlag(c *cli.Command) bool {
	args := c.Args().Slice()
	return len(args) > 0 && strings.HasPrefix(args[len(args)-1], "-")
}

func printWorkflows(c *cli.Command) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	root, err := workflow.FindRoot(cwd)
	if err != nil {
		return
	}
	names, err := workflow.List(root)
	if err != nil {
		return
	}
	for _, name := range names {
		_, _ = fmt.Fprintln(c.Root().Writer, name)
	}
}
