//go:build windows

package setup

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Só Administradores (BA) e SYSTEM (SY), com controle total, sem herdar nada da
// pasta (P = DACL protegida). Por SID, vale em Windows de qualquer idioma.
const somenteAdministradores = "D:P(A;;FA;;;BA)(A;;FA;;;SY)"

// protegerArquivo deixa o arquivo legível só por Administradores e SYSTEM.
//
// Era um `icacls`, que recebia o caminho padrão `/etc/arkame/agent.env.novo`
// e o lia como uma opção (começa com barra): a instalação no Windows parava
// logo depois de o bucket aceitar a chave, com a ajuda do icacls na tela
// (fundador, 28/09). Agora pela API do Windows, com o caminho absoluto — sem
// processo externo, sem aspas e sem depender do idioma da saída.
func protegerArquivo(caminho string) error {
	abs, err := filepath.Abs(caminho)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(somenteAdministradores)
	if err != nil {
		return fmt.Errorf("protegendo %s: %w", abs, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("protegendo %s: %w", abs, err)
	}
	if err := windows.SetNamedSecurityInfo(abs, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("protegendo %s: %w", abs, err)
	}
	return nil
}
