// Package term answers questions about the terminal weft runs in and
// changes its settings.
package term

import (
	"os"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// Width returns the column count of the terminal behind f, or 0 when f is
// not a terminal.
func Width(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

// State is a terminal's settings.
type State = unix.Termios

// GetState reads the settings of the terminal behind f.
func GetState(f *os.File) (*State, error) {
	return unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
}

// SetState applies settings to the terminal behind f.
func SetState(f *os.File, s *State) error {
	return unix.IoctlSetTermios(int(f.Fd()), ioctlSetTermios, s)
}

// WithoutSuspend returns a copy of s in which the suspend key (Ctrl-Z)
// does nothing.
func WithoutSuspend(s *State) *State {
	c := *s
	c.Cc[unix.VSUSP] = vDisable
	return &c
}
