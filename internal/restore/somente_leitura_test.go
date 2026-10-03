package restore

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// A falha por EROFS chega embrulhada (PathError do chmod, "tempfile: %w",
// "mkdir …: %w"); a conferência a acha em qualquer camada, e só ela.
func TestDestinoSomenteLeitura(t *testing.T) {
	for _, err := range []error{
		syscall.EROFS,
		fmt.Errorf("tempfile: %w", syscall.EROFS),
		fmt.Errorf("mkdir /etc/nginx: %w", &os.PathError{Op: "mkdir", Path: "/etc/nginx", Err: syscall.EROFS}),
	} {
		if !DestinoSomenteLeitura(err) {
			t.Errorf("%v não reconhecido como destino só de leitura", err)
		}
	}
	for _, err := range []error{
		syscall.EACCES,
		fmt.Errorf("tempfile: %w", syscall.EPERM),
		errors.New("read-only file system"), // texto não basta: só o erro do sistema
		nil,
	} {
		if DestinoSomenteLeitura(err) {
			t.Errorf("%v reconhecido como destino só de leitura", err)
		}
	}
}
