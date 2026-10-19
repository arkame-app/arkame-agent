//go:build !linux && !darwin && !windows

package caminho

import (
	"errors"
	"os"
)

// DoAberto: sem como saber neste sistema.
func DoAberto(*os.File) (string, error) {
	return "", errors.ErrUnsupported
}
