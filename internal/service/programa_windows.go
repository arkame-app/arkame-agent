//go:build windows

package service

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lerACL e resolverLinks: variáveis para os testes montarem a árvore.
var (
	lerACL = func(p string) (aclDeArquivo, error) {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return aclDeArquivo{}, err
		}
		dono, _, err := sd.Owner()
		if err != nil || dono == nil {
			return aclDeArquivo{}, fmt.Errorf("sem dono: %v", err)
		}
		a := aclDeArquivo{dono: dono.String()}
		dacl, _, err := sd.DACL()
		if err != nil {
			return aclDeArquivo{}, err
		}
		if dacl == nil {
			a.semDACL = true
			return a, nil
		}
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return aclDeArquivo{}, err
			}
			e := aceDeArquivo{tipo: ace.Header.AceType, flags: ace.Header.AceFlags, mascara: uint32(ace.Mask)}
			if e.tipo == tipoAcessoPermitido {
				e.sid = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
			}
			a.aces = append(a.aces, e)
		}
		return a, nil
	}
	resolverLinks = filepath.EvalSymlinks
)

// adminDoInstall é o usuário que roda este install, se ele é administrador
// com o privilégio em uso (elevado: a pertinência ao grupo Administradores só
// vale no token elevado). É o dono do que o setup e o install.ps1 criam
// quando a política de dono padrão é "Criador do objeto". Vazio fora disso.
// Variável para os testes.
var adminDoInstall = func() []string {
	adm, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil
	}
	// Token 0: o CheckTokenMembership usa o token deste processo.
	if eh, err := windows.Token(0).IsMember(adm); err != nil || !eh {
		return nil
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || u.User.Sid == nil {
		return nil
	}
	return []string{u.User.Sid.String()}
}

// conferirPrograma recusa registrar no SCM (o serviço roda como LocalSystem)
// um programa que quem não é administrador pode trocar: quem o trocasse
// viraria SYSTEM no próximo início do serviço. É o `install` rodado de
// Downloads, ou de uma pasta do usuário, sem o setup. O setup e o
// install.ps1 põem o programa em C:\Program Files\Arkame, que passa.
//
// Confere o programa e cada pasta acima dele, até a raiz do volume, no
// caminho dado e no real (links resolvidos) — ver problemaDaACL.
func conferirPrograma(programa string) error {
	abs, err := filepath.Abs(programa)
	if err != nil {
		return fmt.Errorf("conferindo o programa %s: %w", programa, err)
	}
	caminhos := []string{abs}
	if real, err := resolverLinks(abs); err == nil && real != abs {
		caminhos = append(caminhos, real)
	}
	extras := adminDoInstall()
	for _, c := range caminhos {
		papel := papelPrograma
		for p := c; ; p = filepath.Dir(p) {
			var problema string
			if a, err := lerACL(p); err != nil {
				problema = fmt.Sprintf("não consegui ler as permissões de %s (%v)", p, err)
			} else {
				problema = problemaDaACL(p, papel, a, extras)
			}
			if problema != "" {
				return fmt.Errorf(
					"o serviço do Windows roda como SYSTEM e chamaria %s, mas %s: quem pode trocar esse arquivo viraria SYSTEM no próximo início do serviço. "+
						"Instale pelo comando do painel (Windows + R; o setup copia o programa para C:\\Program Files\\Arkame) "+
						"ou pelo install.ps1 num PowerShell como Administrador",
					abs, problema)
			}
			if filepath.Dir(p) == p {
				break
			}
			if papel == papelPrograma {
				papel = papelPastaDoPrograma
			} else {
				papel = papelPastaAcima
			}
		}
	}
	return nil
}
