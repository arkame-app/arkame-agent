//go:build !linux && !darwin && !windows

package terminal

import (
	"errors"
	"os"
)

func openTTY() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

func semEco(*os.File) (func(), error) {
	return nil, errors.New("eco não controlável nesta plataforma")
}
