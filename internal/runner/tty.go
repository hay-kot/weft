package runner

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// reclaimForeground makes weft's process group the terminal's foreground
// group again after an interactive child held it. weft is a background
// process at this point, so tcsetpgrp would raise SIGTTOU and stop weft
// unless the signal is ignored for the call. The signal is ignored only
// here, not around the fork, because ignored signals survive exec and the
// child must get default SIGTTOU behavior.
func reclaimForeground(tty *os.File) {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, unix.Getpgrp())
}
