//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package term

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
	// vDisable is _POSIX_VDISABLE: the control character value that turns a
	// terminal key off.
	vDisable = 0xff
)
