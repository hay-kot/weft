package plain

import (
	"errors"
	"os"
	"time"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/term"
)

// syncTimeout bounds how long an interactive node waits for earlier status
// lines to reach stdout. A paused pipe must not hold the node up forever.
const syncTimeout = 2 * time.Second

// ErrNoTerminal means an interactive node can't run because stdin is not a
// terminal.
var ErrNoTerminal = errors.New("interactive node needs a terminal, but stdin is not a TTY; run weft from a terminal, or skip the node with --only")

// Lender hands the terminal straight to interactive nodes. In plain mode
// weft holds no terminal state, so lending only has to get pending status
// lines out first.
type Lender struct {
	// Sub is the renderer's subscription; nil skips the flush.
	Sub *events.Subscription
}

// Lend runs fn when stdin is a terminal and returns ErrNoTerminal otherwise.
func (l Lender) Lend(fn func()) error {
	if !term.IsTerminal(os.Stdin) {
		return ErrNoTerminal
	}
	if l.Sub != nil {
		l.Sub.Barrier(syncTimeout)
	}
	fn()
	return nil
}
