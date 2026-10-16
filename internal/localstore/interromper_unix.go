//go:build !windows

package localstore

import (
	"os"
	"syscall"
)

// interromper pede ao RustFS para sair (SIGTERM); o WaitDelay mata depois.
func interromper(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
