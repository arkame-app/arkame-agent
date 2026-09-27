//go:build linux || darwin

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

func openTTY() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// semEco desliga o eco e devolve a função que o religa.
func semEco(f *os.File) (func(), error) {
	fd := int(f.Fd())
	antes, err := unix.IoctlGetTermios(fd, ioctlLer)
	if err != nil {
		return nil, err
	}
	depois := *antes
	depois.Lflag &^= unix.ECHO
	depois.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlGravar, &depois); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlGravar, antes) }, nil
}
