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

// donoConfiavel: Administradores, SYSTEM e o usuário deste processo (o
// serviço roda como SYSTEM; um administrador num PowerShell elevado cria
// arquivos em nome de Administradores ou do próprio usuário, conforme a
// política da máquina).
func donoConfiavel(caminho string) (confiavel bool, dono string, err error) {
	abs, err := filepath.Abs(caminho)
	if err != nil {
		return false, "", err
	}
	sd, err := windows.GetNamedSecurityInfo(abs, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, "", err
	}
	sid, _, err := sd.Owner()
	if err != nil {
		return false, "", err
	}
	if sid == nil {
		return false, "(sem dono)", nil
	}
	dono = sid.String()
	for _, tipo := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinLocalSystemSid} {
		if bom, cerr := windows.CreateWellKnownSid(tipo); cerr == nil && sid.Equals(bom) {
			return true, dono, nil
		}
	}
	if eu, uerr := windows.GetCurrentProcessToken().GetTokenUser(); uerr == nil && sid.Equals(eu.User.Sid) {
		return true, dono, nil
	}
	return false, dono, nil
}

// pastaDeVerdade recusa junção, link simbólico e qualquer outro reparse
// point: o dono conferido seria o do destino, e quem controla a pasta de cima
// apontaria o estado do agente para a pasta que quisesse.
func pastaDeVerdade(p string, fi os.FileInfo) error {
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%w: %s é uma junção ou um link", ErrDonoNaoConfiavel, p)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s não é uma pasta", p)
	}
	return nil
}

// permitirEscrita refaz a DACL da pasta: Administradores e SYSTEM com
// controle total, e escritor com modificar (0x1301bf), herdado por arquivos e
// subpastas, sem herdar nada de cima. Pela API, por SID: vale em Windows de
// qualquer idioma e sem processo externo.
func permitirEscrita(dir, escritor string) error {
	sid, _, _, err := windows.LookupSID("", escritor)
	if err != nil {
		return fmt.Errorf("a conta %q não existe neste Windows (para uma instância nomeada do SQL Server, NT Service\\MSSQL$<instância>): %w", escritor, err)
	}
	return aplicar(dir, "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;"+sid.String()+")")
}
