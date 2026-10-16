//go:build !windows

package zimbra

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// travar: uma exportação por pasta. Duas ao mesmo tempo (dois planos com a
// receita, ou o comando rodado à mão durante o plano) apagariam os
// temporários uma da outra.
func travar(pasta string) (func(), error) {
	f, err := os.Open(pasta)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("outra exportação do Zimbra está rodando em %s", pasta)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
