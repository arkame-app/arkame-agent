//go:build !windows

package caminho

import "path/filepath"

// FinalNoWindows, fora do Windows, é o EvalSymlinks (só os testes chamam).
func FinalNoWindows(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
