//go:build !windows

package verificacao

import "golang.org/x/sys/unix"

// espacoLivre devolve os bytes livres, para quem não é root, no sistema de
// arquivos da pasta.
func espacoLivre(pasta string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(pasta, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
