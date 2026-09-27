//go:build windows

package setup

import (
	"fmt"
	"os/exec"
)

// protegerArquivo deixa o arquivo legível só por Administradores e SYSTEM. Por
// SID, para funcionar em Windows em qualquer idioma.
func protegerArquivo(caminho string) error {
	out, err := exec.Command("icacls", caminho, "/inheritance:r", "/grant:r", "*S-1-5-32-544:F", "*S-1-5-18:F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("protegendo %s: %v: %s", caminho, err, out)
	}
	return nil
}
