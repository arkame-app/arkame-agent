//go:build windows

package segredo

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Só Administradores (BA) e SYSTEM (SY), com controle total, sem herdar nada da
// pasta (P = DACL protegida). Por SID, vale em Windows de qualquer idioma.
const somenteAdministradores = "D:P(A;;FA;;;BA)(A;;FA;;;SY)"

// O mesmo para a pasta, herdado por arquivos (OI) e subpastas (CI): o que o
// agente gravar nela — agent.id, log — nasce protegido.
const pastaSomenteAdministradores = "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)"

// protegerComModo aplica a DACL; o modo Unix não significa nada no Windows.
//
// Era um `icacls`, que recebia o caminho padrão `/etc/arkame/agent.env.novo`
// e o lia como uma opção (começa com barra): a instalação no Windows parava
// logo depois de o bucket aceitar a chave (fundador, 28/09). Agora pela API do
// Windows, com o caminho absoluto — sem processo externo, sem aspas e sem
// depender do idioma da saída.
func protegerComModo(caminho string, _ os.FileMode) error {
	return aplicar(caminho, somenteAdministradores)
}

func protegerPasta(dir string) error {
	return aplicar(dir, pastaSomenteAdministradores)
}

func aplicar(caminho, sddl string) error {
	abs, err := filepath.Abs(caminho)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
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
