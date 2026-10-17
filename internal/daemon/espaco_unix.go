//go:build !windows

package daemon

import "syscall"

// espacoDoDisco: os bytes livres e o tamanho do disco da pasta.
func espacoDoDisco(pasta string) (livre, total uint64, ok bool) {
	var st syscall.Statfs_t
	if pasta == "" || syscall.Statfs(pasta, &st) != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), true
}
