package terminal

import "golang.org/x/sys/unix"

const (
	ioctlLer    = unix.TIOCGETA
	ioctlGravar = unix.TIOCSETA
)
