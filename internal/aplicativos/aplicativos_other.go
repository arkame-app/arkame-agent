//go:build !windows

package aplicativos

import (
	"fmt"
	"os"
)

// ElevarSeNecessario: fora do Windows, quem instala roda com sudo.
func ElevarSeNecessario() (bool, error) { return false, nil }

// Registrar não tem o que fazer fora do Windows: o binário é um arquivo só.
func Registrar(string, string, string, []string) error { return nil }

// ProgramaInstalado é onde o install.sh põe o programa.
func ProgramaInstalado() string { return "/usr/local/bin/arkame-agent" }

// AdicionarAoPath: /usr/local/bin já está no PATH.
func AdicionarAoPath(string) error { return nil }

// RemoverEntrada não tem o que fazer fora do Windows.
func RemoverEntrada(string) {}

// RemoverPrograma apaga o programa. Fora do Windows, o arquivo em uso pode sair.
func RemoverPrograma(exe string) error {
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removendo %s: %w", exe, err)
	}
	return nil
}

// ProgramaSaiDepois: fora do Windows, sai na hora.
const ProgramaSaiDepois = false
