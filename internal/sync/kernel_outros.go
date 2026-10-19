//go:build !linux

package sync

import "os"

// doKernel: fora do Linux, /proc e /sys não existem como sistema de arquivos
// de leitura do kernel.
func doKernel(*os.File) bool { return false }
