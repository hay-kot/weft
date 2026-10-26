package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

// RunCmd implements the run command.
type RunCmd struct {
	flags *Flags
}

// NewRunCmd creates a new run command.
func NewRunCmd(flags *Flags) *RunCmd {
	return &RunCmd{flags: flags}
}

// Register adds the run command to the application.
func (cmd *RunCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:  "run",
		Usage: "run command",
		Flags: []cli.Flag{
			// Add command-specific flags here
		},
		Action: cmd.run,
	})

	return app
}

func (cmd *RunCmd) run(_ context.Context, _ *cli.Command) error {
	log.Info().Msg("running run command")
	if cmd.flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"message": "Hello World!",
		})
	}

	fmt.Println("Hello World!")

	return nil
}
