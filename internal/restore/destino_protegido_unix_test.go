//go:build !windows

package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A restauração não grava na pasta do programa do agente nem na da
// configuração dele: é por ali que um painel comprometido poria no servidor um
// programa que a assinatura de release não cobre (a pasta do programa é
// gravável pelo serviço, para a atualização automática trocá-lo).
func TestRestauracaoRecusaAPastaDoProgramaEDaConfiguracao(t *testing.T) {
	comoRoot(t)
	conteudo := []byte("#!/bin/sh\necho programa sem assinatura\n")
	raiz := t.TempDir()
	programa := filepath.Join(raiz, "usr", "local", "bin")
	configuracao := filepath.Join(raiz, "etc", "arkame")
	fora := filepath.Join(raiz, "srv")
	for _, d := range []string{programa, configuracao, fora} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(programa, "arkame-agent"), []byte("programa de verdade"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/", Protegidas: []string{programa, configuracao}}

	recusados := map[string][2]string{
		"o programa":                  {programa, "arkame-agent"},
		"o programa novo da troca":    {programa, "arkame-agent.novo"},
		"o registro da troca":         {programa, "arkame-agent.atualizacao.json"},
		"pelo dest_filename":          {filepath.Dir(programa), "bin/arkame-agent"},
		"subpasta da pasta do agente": {programa, "sub/arkame-agent"},
		"a configuração":              {configuracao, "agent.env"},
		"o programa do volume":        {configuracao, "bin/arkame-agent"},
	}
	for nome, c := range recusados {
		t.Run(nome, func(t *testing.T) {
			item := itemDe(c[0], filepath.Base(c[1]), conteudo, "overwrite")
			item.DestFilename = c[1]
			err := Run(context.Background(), opts, item)
			if !errors.Is(err, ErrDestinoProtegido) {
				t.Fatalf("esperava ErrDestinoProtegido, veio %v", err)
			}
		})
	}
	if b, _ := os.ReadFile(filepath.Join(programa, "arkame-agent")); string(b) != "programa de verdade" {
		t.Fatalf("o programa do agente foi sobrescrito pela restauração: %q", b)
	}
	for _, d := range []string{programa, configuracao} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if e.Name() != "arkame-agent" {
				t.Fatalf("a restauração recusada deixou %s em %s", e.Name(), d)
			}
		}
	}

	// Fora das pastas do agente — inclusive ao lado delas, com nome parecido —
	// a restauração segue normal.
	for _, c := range [][2]string{
		{fora, "arkame-agent"},
		{filepath.Dir(programa), "bin2/arkame-agent"},
		{filepath.Dir(configuracao), "arkame.conf"},
	} {
		item := itemDe(c[0], filepath.Base(c[1]), conteudo, "overwrite")
		item.DestFilename = c[1]
		if err := Run(context.Background(), opts, item); err != nil {
			t.Fatalf("restauração para %s/%s recusada: %v", c[0], c[1], err)
		}
	}
	// Sem pastas protegidas (o executor usado fora do daemon), nada muda.
	item := itemDe(programa, "outro", conteudo, "overwrite")
	if err := Run(context.Background(), Options{S3: opts.S3, HostRoot: "/"}, item); err != nil {
		t.Fatalf("sem pastas protegidas: %v", err)
	}
}

// Um link do sistema que leva à pasta do programa (ou o mesmo diretório por
// outro caminho, como o volume da configuração no Docker): o caminho em texto
// não a denuncia, e a recusa vem da pasta aberta, pela identidade dela.
func TestRestauracaoRecusaAPastaDoAgentePorOutroCaminho(t *testing.T) {
	// Sem comoRoot: o link é do próprio agente, numa pasta que só ele grava —
	// é um link do sistema, que a restauração segue.
	conteudo := []byte("#!/bin/sh\necho programa sem assinatura\n")
	raiz := t.TempDir()
	programa := filepath.Join(raiz, "opt", "arkame")
	fora := filepath.Join(raiz, "srv")
	for _, d := range []string{programa, fora} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(programa, filepath.Join(fora, "atalho")); err != nil {
		t.Fatal(err)
	}
	opts := Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/", Protegidas: []string{programa}}
	for nome, c := range map[string][2]string{
		"a pasta de destino é o link": {filepath.Join(fora, "atalho"), "arkame-agent"},
		"o link no dest_filename":     {fora, "atalho/arkame-agent"},
		"subpasta depois do link":     {fora, "atalho/sub/arkame-agent"},
	} {
		t.Run(nome, func(t *testing.T) {
			item := itemDe(c[0], filepath.Base(c[1]), conteudo, "overwrite")
			item.DestFilename = c[1]
			if err := Run(context.Background(), opts, item); !errors.Is(err, ErrDestinoProtegido) {
				t.Fatalf("esperava ErrDestinoProtegido, veio %v", err)
			}
			if _, err := os.Lstat(filepath.Join(programa, "arkame-agent")); err == nil {
				t.Fatal("a restauração gravou na pasta do programa pelo link")
			}
		})
	}
	// A premissa: sem a proteção, o mesmo caminho grava lá.
	item := itemDe(filepath.Join(fora, "atalho"), "outro", conteudo, "overwrite")
	if err := Run(context.Background(), Options{S3: opts.S3, HostRoot: "/"}, item); err != nil {
		t.Fatalf("premissa (o link do sistema é seguido): %v", err)
	}
}

func TestDentroDeProtegidaPeloTexto(t *testing.T) {
	protegidas := []string{"/usr/local/bin", "/etc/arkame", ""}
	for alvo, quer := range map[string]bool{
		"/usr/local/bin":          true,
		"/usr/local/bin/":         true,
		"/usr/local/bin/sub":      true,
		"/usr/local/./bin/../bin": true,
		"/etc/arkame/bin":         true,
		"/usr/local/bin2":         false,
		"/usr/local":              false,
		"/etc":                    false,
		"/etc/arkame2":            false,
		"/srv/usr/local/bin":      false,
	} {
		if _, got := dentroDeProtegida(alvo, protegidas); got != quer {
			t.Errorf("dentroDeProtegida(%q) = %v, queria %v", alvo, got, quer)
		}
	}
}
