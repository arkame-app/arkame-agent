//go:build linux

package caminho

import (
	"os"
	"strconv"
)

// DoAberto devolve o caminho real do arquivo já aberto: o que foi de fato
// lido, mesmo que uma pasta no meio tenha virado link depois da varredura.
func DoAberto(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
