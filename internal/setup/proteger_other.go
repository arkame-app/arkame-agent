//go:build !windows

package setup

import "os"

// protegerArquivo: 0600, só o dono (root) lê.
func protegerArquivo(caminho string) error { return os.Chmod(caminho, 0o600) }
