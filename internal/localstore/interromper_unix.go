//go:build !windows

package localstore

import (
	"os"
	"syscall"
)

// interromper pede ao RustFS para sair (SIGTERM); o WaitDelay mata depois.
func interromper(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// semSeguirLink: abrir o log sem seguir link simbólico.
const semSeguirLink = syscall.O_NOFOLLOW

// prenderAoAgente: fora do Windows o filho já recebe o SIGTERM do supervisor,
// e o systemd/launchd mata o grupo do serviço inteiro.
func prenderAoAgente(*os.Process) (func(), error) { return func() {}, nil }
