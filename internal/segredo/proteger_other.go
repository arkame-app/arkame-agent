//go:build !windows

package segredo

import "os"

func protegerComModo(caminho string, modo os.FileMode) error {
	return os.Chmod(caminho, modo)
}

// protegerPasta: o 0700 do MkdirAll já basta.
func protegerPasta(string) error { return nil }
