package service

import (
	"strings"
	"testing"
)

const (
	sidUsuarios             = "S-1-5-32-545"
	sidUsuariosAutenticados = "S-1-5-11"
	sidAna                  = "S-1-5-21-1-2-3-1001"
	leituraEExecucao        = 0x1200a9
	modificar               = 0x1301bf
)

// O que o Windows põe em C:\Program Files\Arkame (herdado do Program Files) e
// no programa que o setup copia para lá: passa.
func TestACLDoProgramFilesPassa(t *testing.T) {
	herdadas := []aceDeArquivo{
		{mascara: 0x1f01ff, sid: sidSystem},
		{mascara: 0x1f01ff, sid: sidAdministradores},
		{mascara: 0x1f01ff, sid: sidTrustedInstaller},
		{mascara: leituraEExecucao, sid: sidUsuarios},
		{flags: aceSoParaHerdar, mascara: acessoGenericoTotal, sid: sidCreatorOwner},
	}
	for _, papel := range []papelNoCaminho{papelPrograma, papelPastaDoPrograma, papelPastaAcima} {
		if p := problemaDaACL(`C:\Program Files\Arkame`, papel, aclDeArquivo{dono: sidAdministradores, aces: herdadas}); p != "" {
			t.Errorf("papel %d recusado: %s", papel, p)
		}
	}
	// A raiz do C:\: Usuários Autenticados criam pasta (0x4) e têm Modificar
	// só para herdar. Não troca o que está embaixo.
	raiz := aclDeArquivo{dono: sidTrustedInstaller, aces: []aceDeArquivo{
		{mascara: 0x1f01ff, sid: sidSystem},
		{mascara: 0x1f01ff, sid: sidAdministradores},
		{mascara: leituraEExecucao, sid: sidUsuarios},
		{mascara: acessoAcrescentar, sid: sidUsuariosAutenticados},
		{flags: aceSoParaHerdar | 0x3, mascara: modificar, sid: sidUsuariosAutenticados},
	}}
	if p := problemaDaACL(`C:\`, papelPastaAcima, raiz); p != "" {
		t.Errorf("raiz do C:\\ recusada: %s", p)
	}
}

// Downloads da ana: dona é ela, e ela tem Controle Total. O serviço SYSTEM
// chamaria um arquivo que qualquer processo dela troca.
func TestACLRecusaQuemNaoEAdministrador(t *testing.T) {
	casos := []struct {
		nome  string
		papel papelNoCaminho
		acl   aclDeArquivo
		quer  string
	}{
		{"dono é o usuário", papelPrograma,
			aclDeArquivo{dono: sidAna, aces: []aceDeArquivo{{mascara: 0x1f01ff, sid: sidAdministradores}}}, "o dono de"},
		{"usuário com controle total no programa", papelPrograma,
			aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{mascara: 0x1f01ff, sid: sidAna}}}, sidAna},
		{"Usuários gravam no programa", papelPrograma,
			aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{mascara: acessoGravarArquivo, sid: sidUsuarios}}}, sidUsuarios},
		{"Usuários põem arquivo na pasta do programa", papelPastaDoPrograma,
			aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{mascara: acessoGravarArquivo, sid: sidUsuarios}}}, sidUsuarios},
		{"Usuários Autenticados apagam filho numa pasta acima", papelPastaAcima,
			aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{mascara: modificar | acessoApagarFilho, sid: sidUsuariosAutenticados}}}, sidUsuariosAutenticados},
		{"Modificar herdado numa pasta acima (DELETE)", papelPastaAcima,
			aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{flags: 0x10, mascara: modificar, sid: sidUsuariosAutenticados}}}, sidUsuariosAutenticados},
		{"DACL nula", papelPastaAcima,
			aclDeArquivo{dono: sidAdministradores, semDACL: true}, "qualquer usuário"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			p := problemaDaACL(`C:\x`, c.papel, c.acl)
			if p == "" || !strings.Contains(p, c.quer) {
				t.Fatalf("queria a recusa com %q, veio %q", c.quer, p)
			}
		})
	}
}

// Entrada de negação não dá acesso, e só-para-herdar não vale para o caminho.
func TestACLIgnoraNegacaoESoParaHerdar(t *testing.T) {
	a := aclDeArquivo{dono: sidSystem, aces: []aceDeArquivo{
		{tipo: 1, mascara: 0x1f01ff, sid: sidAna},
		{flags: aceSoParaHerdar, mascara: 0x1f01ff, sid: sidAna},
	}}
	if p := problemaDaACL(`C:\x`, papelPrograma, a); p != "" {
		t.Fatalf("recusado: %s", p)
	}
}
