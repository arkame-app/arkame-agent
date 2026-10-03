//go:build !windows

package restore

import "os"

// copiarDono dá ao arquivo o dono e o grupo do existente. Só como root: outro
// usuário não pode dar o arquivo a terceiros, e o que ele cria já é dele.
func copiarDono(f *os.File, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return f.Chown(uid, gid)
}
