//go:build !windows

package restore

import (
	"time"

	"golang.org/x/sys/unix"
)

// aplicarData grava a data de modificação (e a de acesso) do arquivo.
//
// Não é o os.Chtimes: ele converte por UnixNano, que só cobre 1677–2262, e a
// data restaurada de um arquivo fora disso dava a volta. Aqui vão segundos e
// nanossegundos separados.
func aplicarData(caminho string, acesso, modificacao time.Time) error {
	a, err := unix.TimeToTimespec(acesso)
	if err != nil {
		return err
	}
	m, err := unix.TimeToTimespec(modificacao)
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, caminho, []unix.Timespec{a, m}, 0)
}
