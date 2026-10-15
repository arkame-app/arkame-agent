//go:build !windows

package instantaneo

import (
	"errors"
	"os"
)

// caminhoExato: fora do Windows não há instantâneo, e o sistema de arquivos
// diferencia maiúsculas; basta existir.
func caminhoExato(p string) (bool, error) {
	_, err := os.Lstat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
