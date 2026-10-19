//go:build darwin

package caminho

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DoAberto devolve o caminho real do arquivo já aberto (F_GETPATH).
func DoAberto(f *os.File) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, _, e := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return "", e
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf), nil
}
