//go:build linux

package sync

import (
	"os"

	"golang.org/x/sys/unix"
)

// doKernel: o arquivo aberto está num sistema de arquivos do kernel, pela
// assinatura do sistema (fstatfs), e não pelo caminho.
func doKernel(f *os.File) bool {
	var s unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &s) != nil {
		return false
	}
	switch int64(s.Type) {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEBUGFS_MAGIC, unix.SECURITYFS_MAGIC, unix.TRACEFS_MAGIC, unix.BPF_FS_MAGIC:
		return true
	}
	return false
}
