package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

// ValidateCmd implements the validate command.
type ValidateCmd struct {
	flags *Flags
}

// NewValidateCmd creates a new validate command.
func NewValidateCmd(flags *Flags) *ValidateCmd {
	return &ValidateCmd{flags: flags}
}

// Register adds the validate command to the application.
func (cmd *ValidateCmd) Register(app *cli.Command) *cli.Command {
	app.Commands = append(app.Commands, &cli.Command{
		Name:  "validate",
		Usage: "validate command",
		Flags: []cli.Flag{
			// Add command-specific flags here
		},
		Action: cmd.run,
	})

	return app
}

func (cmd *ValidateCmd) run(_ context.Context, _ *cli.Command) error {
	log.Info().Msg("running validate command")
	if cmd.flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"message": "Hello World!",
		})
	}

	fmt.Println("Hello World!")

	return nil
}
