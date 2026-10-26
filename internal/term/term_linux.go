package term

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
	// vDisable is _POSIX_VDISABLE: the control character value that turns a
	// terminal key off.
	vDisable = 0
)
