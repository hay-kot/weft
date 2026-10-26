package plain

import (
	"errors"
	"os"

	"github.com/hay-kot/weft/internal/runner"
)

// ErrNoTerminal means an interactive node can't run because stdin is not a
// terminal.
var ErrNoTerminal = errors.New("interactive node needs a terminal, but stdin is not a TTY; run weft from a terminal, or skip the node with --only")

// Lender hands the terminal straight to interactive nodes. In plain mode
// weft holds no terminal state, so lending only has to make sure pending
// status lines are out first.
type Lender struct {
	Renderer *Renderer
}

// Lend runs fn when stdin is a terminal and returns ErrNoTerminal otherwise.
func (l Lender) Lend(fn func()) error {
	if !runner.IsTerminal(os.Stdin) {
		return ErrNoTerminal
	}
	if l.Renderer != nil {
		l.Renderer.Sync()
	}
	fn()
	return nil
}
