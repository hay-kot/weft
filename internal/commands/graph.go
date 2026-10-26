package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

// GraphCmd implements the graph command.
type GraphCmd struct {
	flags *Flags
}

// NewGraphCmd creates a new graph command.
func NewGraphCmd(flags *Flags) *GraphCmd {
	return &GraphCmd{flags: flags}
}

// Register adds the graph command to the application.
func (cmd *GraphCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:  "graph",
		Usage: "graph command",
		Flags: []cli.Flag{
			// Add command-specific flags here
		},
		Action: cmd.run,
	})

	return app
}

func (cmd *GraphCmd) run(_ context.Context, _ *cli.Command) error {
	log.Info().Msg("running graph command")
	if cmd.flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"message": "Hello World!",
		})
	}

	fmt.Println("Hello World!")

	return nil
}
