//go:build !windows

package aplicativos

import (
	"fmt"
	"os"
)

// ElevarSeNecessario: fora do Windows, quem instala roda com sudo.
func ElevarSeNecessario() (bool, error) { return false, nil }

// Registrar não tem o que fazer fora do Windows: o binário é um arquivo só.
func Registrar(string, string) error { return nil }

// Remover apaga o programa. Fora do Windows, o arquivo em uso pode sair.
func Remover(exe string) error {
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removendo %s: %w", exe, err)
	}
	return nil
}
