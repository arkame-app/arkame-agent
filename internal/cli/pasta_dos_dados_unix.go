//go:build !windows

package cli

import "os"

// pastaAberta: outro usuário (grupo ou demais) lê ou entra na pasta.
func pastaAberta(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.Mode().Perm()&0o077 != 0, nil
}

func fecharPasta(p string) error { return os.Chmod(p, 0o700) }
