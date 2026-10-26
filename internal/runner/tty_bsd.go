//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package runner

import "golang.org/x/sys/unix"

const ioctlGetTermios = unix.TIOCGETA
