// Package term answers questions about the terminal weft runs in.
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
