//go:build !windows

package restore

import (
	"time"

	"golang.org/x/sys/unix"
)

// aplicarData grava a data de modificação (e a de acesso) do arquivo nome da
// pasta dirFd, sem seguir link.
//
// Não é o os.Chtimes: ele converte por UnixNano, que só cobre 1677–2262, e a
// data restaurada de um arquivo fora disso dava a volta. Aqui vão segundos e
// nanossegundos separados.
func aplicarData(dirFd int, nome string, acesso, modificacao time.Time) error {
	a, err := unix.TimeToTimespec(acesso)
	if err != nil {
		return err
	}
	m, err := unix.TimeToTimespec(modificacao)
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(dirFd, nome, []unix.Timespec{a, m}, unix.AT_SYMLINK_NOFOLLOW)
}
