package service

import "fmt"

// Conferência do programa do serviço do Windows pela ACL, separada da leitura
// (programa_windows.go) para os testes rodarem em qualquer sistema.

// SIDs em que o serviço do Windows (LocalSystem) pode confiar como dono ou com
// escrita no caminho do programa: SYSTEM, Administradores e TrustedInstaller.
const (
	sidSystem           = "S-1-5-18"
	sidAdministradores  = "S-1-5-32-544"
	sidTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	// CREATOR OWNER e OWNER RIGHTS valem para o dono, conferido à parte.
	sidCreatorOwner = "S-1-3-0"
	sidOwnerRights  = "S-1-3-4"
)

// Bits do ACCESS_MASK (winnt.h) que importam aqui.
const (
	acessoGravarArquivo    = 0x2        // FILE_WRITE_DATA / FILE_ADD_FILE
	acessoAcrescentar      = 0x4        // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	acessoApagarFilho      = 0x40       // FILE_DELETE_CHILD
	acessoApagar           = 0x10000    // DELETE
	acessoGravarDACL       = 0x40000    // WRITE_DAC
	acessoGravarDono       = 0x80000    // WRITE_OWNER
	acessoGenericoTotal    = 0x10000000 // GENERIC_ALL
	acessoGenericoGravar   = 0x40000000 // GENERIC_WRITE
	tipoAcessoPermitido    = 0          // ACCESS_ALLOWED_ACE_TYPE
	aceSoParaHerdar        = 0x8        // INHERIT_ONLY_ACE
	acessoQueTrocaQualquer = acessoApagar | acessoGravarDACL | acessoGravarDono | acessoGenericoTotal | acessoGenericoGravar
)

// papelNoCaminho diz o que um caminho é para o programa, e daí que escrita o
// ameaça.
type papelNoCaminho int

const (
	// O próprio programa: gravar nele é trocá-lo.
	papelPrograma papelNoCaminho = iota
	// A pasta do programa: além de apagar ou renomear o programa
	// (FILE_DELETE_CHILD), pôr um arquivo nela basta (uma DLL que o
	// programa carrega da própria pasta).
	papelPastaDoPrograma
	// Pasta mais acima: só apagar ou renomear o que está embaixo ameaça.
	// Criar pasta nova não — é o que a raiz do C:\ dá aos Usuários
	// Autenticados.
	papelPastaAcima
)

// aceDeArquivo é uma entrada da DACL: tipo, flags, máscara e o SID em texto.
type aceDeArquivo struct {
	tipo    byte
	flags   byte
	mascara uint32
	sid     string
}

// aclDeArquivo é o que a conferência lê de cada caminho: o dono e a DACL.
// semDACL é a DACL nula, que dá tudo a todos.
type aclDeArquivo struct {
	dono    string
	semDACL bool
	aces    []aceDeArquivo
}

func sidConfiavel(sid string) bool {
	switch sid {
	case sidSystem, sidAdministradores, sidTrustedInstaller:
		return true
	}
	return false
}

// problemaDaACL descreve por que caminho, no papel dado, pode ser trocado por
// quem não é administrador; vazio se não pode. O dono tem de ser confiável
// (o dono sempre pode reescrever a DACL), e nenhuma entrada que permite
// escrita perigosa pode ser de outro SID. Entradas só para herdar não valem
// para o próprio caminho; as de negação são ignoradas (o que pode errar só
// para o lado de recusar).
func problemaDaACL(caminho string, papel papelNoCaminho, a aclDeArquivo) string {
	if !sidConfiavel(a.dono) {
		return fmt.Sprintf("o dono de %s é %s, e não os Administradores, o SYSTEM ou o TrustedInstaller", caminho, a.dono)
	}
	if a.semDACL {
		return fmt.Sprintf("%s não tem lista de permissões (qualquer usuário grava)", caminho)
	}
	perigosa := uint32(acessoQueTrocaQualquer)
	switch papel {
	case papelPrograma:
		perigosa |= acessoGravarArquivo | acessoAcrescentar
	case papelPastaDoPrograma:
		perigosa |= acessoApagarFilho | acessoGravarArquivo
	case papelPastaAcima:
		perigosa |= acessoApagarFilho
	}
	for _, e := range a.aces {
		if e.tipo != tipoAcessoPermitido || e.flags&aceSoParaHerdar != 0 {
			continue
		}
		if sidConfiavel(e.sid) || e.sid == sidCreatorOwner || e.sid == sidOwnerRights {
			continue
		}
		if e.mascara&perigosa != 0 {
			return fmt.Sprintf("%s pode ser alterado por %s (permissão 0x%x)", caminho, e.sid, e.mascara)
		}
	}
	return ""
}
