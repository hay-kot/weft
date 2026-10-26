package runner

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/hay-kot/weft/internal/term"
)

// lendTerminal readies tty for an interactive child and returns the
// function that takes it back. It is called before the child starts: the
// child takes the foreground process group before it execs, so a start that
// fails after that point (a missing dir) would otherwise leave the terminal
// with a dead foreground group, and weft's next write would stop it with
// SIGTTOU.
//
// While the child has the terminal, Ctrl-Z does nothing. weft does not hand
// its jobs to the shell, so a stopped node would leave the terminal with a
// foreground group nothing can resume. The settings are put back afterwards,
// so a node killed in the middle of a hidden-input prompt does not leave
// echo off.
func lendTerminal(tty *os.File) (restore func(), err error) {
	// A weft in the background (`weft run x &`) can't hand over a terminal
	// it does not hold; its tcsetattr below would stop it with SIGTTOU.
	if fg, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP); err == nil && fg != unix.Getpgrp() {
		return nil, errors.New("weft is not the terminal's foreground job, so it can't hand the terminal to an interactive node; bring it to the foreground")
	}
	saved, err := term.GetState(tty)
	if err != nil {
		return nil, fmt.Errorf("read terminal settings: %w", err)
	}
	if err := term.SetState(tty, term.WithoutSuspend(saved)); err != nil {
		return nil, fmt.Errorf("set terminal settings: %w", err)
	}
	return func() {
		// weft is a background process at this point, so tcsetpgrp and
		// tcsetattr would raise SIGTTOU and stop weft unless the signal is
		// ignored for the calls. It is ignored only here, not around the
		// fork, because ignored signals survive exec and the child must get
		// default SIGTTOU behavior.
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
		_ = unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, unix.Getpgrp())
		_ = term.SetState(tty, saved)
	}, nil
}
