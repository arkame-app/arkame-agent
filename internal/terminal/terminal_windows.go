//go:build windows

package terminal

import (
	"os"

	"golang.org/x/sys/windows"
)

func openTTY() (*os.File, error) { return os.OpenFile("CONIN$", os.O_RDWR, 0) }

// semEco desliga o eco do console e devolve a função que o religa.
func semEco(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	var modo uint32
	if err := windows.GetConsoleMode(h, &modo); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(h, modo&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(h, modo) }, nil
}
