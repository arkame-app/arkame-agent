//go:build !windows

package cli

import "syscall"

// espacoLivreEm: os bytes livres do disco da pasta.
func espacoLivreEm(p string) (uint64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
