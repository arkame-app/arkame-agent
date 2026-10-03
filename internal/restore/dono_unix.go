//go:build !windows

package restore

import (
	"os"
	"syscall"
)

// copiarDono dá ao arquivo o dono e o grupo do existente. Só como root: outro
// usuário não pode dar o arquivo a terceiros, e o que ele cria já é dele.
func copiarDono(f *os.File, existente os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, ok := existente.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return f.Chown(int(st.Uid), int(st.Gid))
}
