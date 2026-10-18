//go:build windows

package caminho

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// FinalNoWindows devolve o caminho final de p, com junções e links seguidos
// pelo próprio Windows (GetFinalPathNameByHandle). O filepath.EvalSymlinks do
// Go 1.23+ não atravessa junções (winsymlink=1), e `C:\dados\j\RegBack`, com
// `j -> C:\Windows\System32\config`, passava pela conferência. Sem como abrir,
// fica o EvalSymlinks, e sem ele o próprio p.
func FinalNoWindows(p string) string {
	if final, err := finalPeloHandle(p); err == nil {
		return final
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func finalPeloHandle(p string) (string, error) {
	nome, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	// Só atributos (sem ler dados), e BACKUP_SEMANTICS para abrir pasta.
	h, err := windows.CreateFile(nome, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			final := windows.UTF16ToString(buf[:n])
			// `\\?\C:\...` → `C:\...`; `\\?\UNC\srv\x` → `\\srv\x`.
			if resto, ok := strings.CutPrefix(final, `\\?\UNC\`); ok {
				return `\\` + resto, nil
			}
			return strings.TrimPrefix(final, `\\?\`), nil
		}
		buf = make([]uint16, n+1)
	}
}
