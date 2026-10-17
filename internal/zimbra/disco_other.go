//go:build !windows

package zimbra

import "syscall"

// espacoNoDisco: os bytes livres (para quem não é root, como o df) e o
// tamanho do disco onde está a pasta.
func espacoNoDiscoDoSistema(pasta string) (livre, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(pasta, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
