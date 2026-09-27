package terminal

import "golang.org/x/sys/unix"

const (
	ioctlLer    = unix.TCGETS
	ioctlGravar = unix.TCSETS
)
