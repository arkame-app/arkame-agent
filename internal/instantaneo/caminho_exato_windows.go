//go:build windows

package instantaneo

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// caminhoExato abre p sem seguir ponto de reparse e compara o caminho que o
// sistema dá ao handle com p, letra por letra.
func caminhoExato(p string) (bool, error) {
	nome, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(nome, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0) // FILE_NAME_NORMALIZED|VOLUME_NAME_DOS, os dois 0
	if err != nil {
		return false, err
	}
	final := strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
	// A letra da unidade pode vir em outra caixa; o resto não.
	return len(final) == len(p) && strings.EqualFold(final[:2], p[:2]) && final[2:] == p[2:], nil
}
