package commands

import (
	"context"
	_ "embed"
	"errors"
	"io"

	"github.com/urfave/cli/v3"
)

//go:embed doc.md
var agentDoc string

// DocCmd implements the doc command.
type DocCmd struct {
	flags *Flags
}

// NewDocCmd creates a new doc command.
func NewDocCmd(flags *Flags) *DocCmd {
	return &DocCmd{flags: flags}
}

// Register adds the doc command to the application.
func (cmd *DocCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:  "doc",
		Usage: "print a compact reference for agents and scripts",
		Description: `Prints, as Markdown, what an agent needs to drive weft: rules of thumb for
running workflows safely without a terminal, every command, how to read a
failed run, the JSON shapes, run statuses, node states and reasons, the run
directory layout, and exit codes. It ships in the binary, so it matches the
weft you run.

Examples:
  weft doc
  weft doc > .weft/AGENTS.md`,
		Action: cmd.run,
	})
	return app
}

func (cmd *DocCmd) run(_ context.Context, c *cli.Command) error {
	if c.NArg() != 0 {
		return errors.New("usage: weft doc")
	}
	_, err := io.WriteString(c.Root().Writer, agentDoc)
	return err
}
