//go:build !windows

package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arvore troca o lerDono e o resolverLinks por uma árvore montada no teste.
func arvore(t *testing.T, nos map[string]donoDoCaminho, links map[string]string) {
	t.Helper()
	antesDono, antesLinks := lerDono, resolverLinks
	t.Cleanup(func() { lerDono, resolverLinks = antesDono, antesLinks })
	lerDono = func(p string) (donoDoCaminho, error) {
		if d, ok := nos[p]; ok {
			return d, nil
		}
		return donoDoCaminho{}, fs.ErrNotExist
	}
	resolverLinks = func(p string) (string, error) {
		if r, ok := links[p]; ok {
			return r, nil
		}
		return p, nil
	}
}

func raizDoSistema() map[string]donoDoCaminho {
	return map[string]donoDoCaminho{
		"/":                                 {modo: fs.ModeDir | 0o755},
		"/usr":                              {modo: fs.ModeDir | 0o755},
		"/usr/local":                        {modo: fs.ModeDir | 0o755},
		"/usr/local/bin":                    {modo: fs.ModeDir | 0o755},
		"/usr/local/bin/arkame-agent":       {modo: 0o755},
		"/home":                             {modo: fs.ModeDir | 0o755},
		"/home/ana":                         {uid: 1000, gid: 1000, modo: fs.ModeDir | 0o700},
		"/home/ana/.local":                  {uid: 1000, gid: 1000, modo: fs.ModeDir | 0o755},
		"/home/ana/.local/bin":              {uid: 1000, gid: 1000, modo: fs.ModeDir | 0o755},
		"/home/ana/.local/bin/arkame-agent": {uid: 1000, gid: 1000, modo: 0o755},
	}
}

// Serviço root com ExecStart no home do usuário: quem troca o arquivo vira
// root no próximo reinício. O programa do sudo (/usr/local/bin) passa.
func TestServicoDoSistemaRecusaProgramaQueOutroUsuarioTroca(t *testing.T) {
	nos := raizDoSistema()
	arvore(t, nos, nil)
	if err := conferirPrograma("/usr/local/bin/arkame-agent"); err != nil {
		t.Fatalf("/usr/local/bin do root recusado: %v", err)
	}
	err := conferirPrograma("/home/ana/.local/bin/arkame-agent")
	if err == nil {
		t.Fatal("programa no home da ana aceito para o serviço root")
	}
	for _, trecho := range []string{"pertence a", "/usr/local/bin", "escopo user", "/home/ana/.local/bin/arkame-agent install --token="} {
		if !strings.Contains(err.Error(), trecho) {
			t.Errorf("a mensagem não diz %q: %v", trecho, err)
		}
	}

	// Arquivo do root, mas numa pasta que a ana grava: ela o renomeia.
	nos["/home/ana/.local/bin/arkame-agent"] = donoDoCaminho{modo: 0o755}
	if err := conferirPrograma("/home/ana/.local/bin/arkame-agent"); err == nil {
		t.Fatal("programa do root numa pasta da ana aceito")
	}
}

func TestServicoDoSistemaRecusaProgramaGravavelPorGrupoOuOutros(t *testing.T) {
	casos := []struct {
		nome   string
		no     string
		dono   donoDoCaminho
		recusa string
	}{
		{"programa gravável pelo grupo", "/usr/local/bin/arkame-agent", donoDoCaminho{gid: 50, modo: 0o775}, "gravável pelo grupo"},
		{"programa gravável por todos", "/usr/local/bin/arkame-agent", donoDoCaminho{modo: 0o757}, "qualquer usuário"},
		{"pasta gravável pelo grupo staff", "/usr/local", donoDoCaminho{gid: 50, modo: fs.ModeDir | 0o2775}, "gravável pelo grupo"},
		{"pasta gravável por todos", "/usr/local/bin", donoDoCaminho{modo: fs.ModeDir | 0o1777}, "qualquer usuário"},
		{"grupo do root pode gravar", "/usr/local/bin", donoDoCaminho{gid: 0, modo: fs.ModeDir | 0o775}, ""},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			nos := raizDoSistema()
			nos[c.no] = c.dono
			arvore(t, nos, nil)
			err := conferirPrograma("/usr/local/bin/arkame-agent")
			if c.recusa == "" {
				if err != nil {
					t.Fatalf("recusado: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.recusa) {
				t.Fatalf("queria a recusa %q, veio %v", c.recusa, err)
			}
		})
	}
}

// Link do root em /usr/local/bin apontando para o home: vale o arquivo real.
func TestServicoDoSistemaConfereODestinoDoLink(t *testing.T) {
	nos := raizDoSistema()
	nos["/usr/local/bin/arkame-agent"] = donoDoCaminho{modo: fs.ModeSymlink | 0o777}
	arvore(t, nos, map[string]string{"/usr/local/bin/arkame-agent": "/home/ana/.local/bin/arkame-agent"})
	err := conferirPrograma("/usr/local/bin/arkame-agent")
	if err == nil || !strings.Contains(err.Error(), "/home/ana") {
		t.Fatalf("link para o home da ana aceito: %v", err)
	}

	// O link do root para um programa do root passa, mesmo com 0777 (link
	// não tem permissão própria).
	nos["/opt"] = donoDoCaminho{modo: fs.ModeDir | 0o755}
	nos["/opt/arkame-agent"] = donoDoCaminho{modo: 0o755}
	arvore(t, nos, map[string]string{"/usr/local/bin/arkame-agent": "/opt/arkame-agent"})
	if err := conferirPrograma("/usr/local/bin/arkame-agent"); err != nil {
		t.Fatalf("link do root para /opt recusado: %v", err)
	}
}

// No escopo user o serviço roda como o dono do programa: nada a conferir.
func TestEscopoUserNaoConfereODono(t *testing.T) {
	arvore(t, map[string]donoDoCaminho{}, nil)
	lerDono = func(string) (donoDoCaminho, error) { return donoDoCaminho{}, errors.New("não devia ler") }
	if err := ConferirPrograma(ScopeUser, "/home/ana/.local/bin/arkame-agent"); err != nil {
		t.Fatal(err)
	}
}

// No disco de verdade: um programa numa pasta temporária do usuário (não
// root) é recusado para o serviço do sistema.
func TestServicoDoSistemaRecusaProgramaDoUsuarioNoDisco(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("como root, a pasta temporária é do root")
	}
	p := filepath.Join(t.TempDir(), "arkame-agent")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ConferirPrograma(ScopeSystem, p); err == nil {
		t.Fatalf("%s, do usuário, aceito para o serviço root", p)
	}
}

// No Mac Intel com Homebrew, o /usr/local/bin é do usuário: a recusa mandava
// rodar o mesmo `| sudo sh` que acabara de falhar. Agora diz onde o
// instalador põe nesse caso e como escolher a pasta (ARKAME_BIN_DIR).
func TestRecusaDoProgramaDizComoEscolherAPasta(t *testing.T) {
	arvore(t, raizDoSistema(), nil)
	err := conferirPrograma("/home/ana/.local/bin/arkame-agent")
	if err == nil {
		t.Fatal("programa no home da ana aceito para o serviço root")
	}
	for _, trecho := range []string{"/opt/arkame/bin", "sudo env ARKAME_BIN_DIR="} {
		if !strings.Contains(err.Error(), trecho) {
			t.Errorf("a mensagem não diz %q: %v", trecho, err)
		}
	}
}
