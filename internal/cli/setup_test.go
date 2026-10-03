package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A troca do programa: o novo entra, o antigo sai, e nada pela metade fica.
func TestCopiarPrograma(t *testing.T) {
	d := t.TempDir()
	de, para := filepath.Join(d, "baixado.exe"), filepath.Join(d, "Arkame", "arkame-agent.exe")
	if err := os.WriteFile(de, []byte("novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copiarPrograma(de, para); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(de, []byte("mais novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copiarPrograma(de, para); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(para); string(b) != "mais novo" {
		t.Fatalf("programa = %q", b)
	}
	for _, sobra := range []string{para + ".novo", para + ".old"} {
		if _, err := os.Stat(sobra); !os.IsNotExist(err) {
			t.Fatalf("sobrou %s", sobra)
		}
	}
}

// ocupar imita, fora do Windows, um .old que não sai (o exe que outro serviço
// ainda roda): um diretório com conteúdo, que nem os.Remove apaga nem um
// rename de arquivo substitui.
func ocupar(t *testing.T, caminho string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(caminho, "em-uso"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// O .old anterior ainda em uso (dois agentes no mesmo exe, ou a janela fechada
// no meio do setup) não pode travar a troca: o atual sai com outro nome.
func TestCopiarProgramaComOldEmUso(t *testing.T) {
	d := t.TempDir()
	de, para := filepath.Join(d, "baixado.exe"), filepath.Join(d, "arkame-agent.exe")
	for _, a := range []struct{ caminho, corpo string }{{de, "novo"}, {para, "velho"}} {
		if err := os.WriteFile(a.caminho, []byte(a.corpo), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ocupar(t, para+".old")
	if err := copiarPrograma(de, para); err != nil {
		t.Fatalf("a troca falhou com o .old em uso: %v", err)
	}
	if b, _ := os.ReadFile(para); string(b) != "novo" {
		t.Fatalf("programa = %q", b)
	}
	if _, err := os.Stat(para + ".novo"); !os.IsNotExist(err) {
		t.Fatal("sobrou o .novo")
	}
}

// Na entrada saem todos os .old* livres — inclusive os de nome único de
// trocas anteriores — e só eles: o que está em uso e os vizinhos ficam.
func TestCopiarProgramaLimpaOsOldLivres(t *testing.T) {
	d := t.TempDir()
	de, para := filepath.Join(d, "baixado.exe"), filepath.Join(d, "arkame-agent.exe")
	vizinhos := []string{
		filepath.Join(d, "arkame-agent.exe.config"),
		filepath.Join(d, "outro.exe.old"),
		para + ".oldie",
	}
	livres := []string{para + ".old", para + ".old-1a2b", para + ".old-ffff"}
	for _, a := range append(append([]string{de, para}, vizinhos...), livres...) {
		if err := os.WriteFile(a, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	emUso := para + ".old-c0de"
	ocupar(t, emUso)
	if err := copiarPrograma(de, para); err != nil {
		t.Fatal(err)
	}
	for _, a := range livres {
		if _, err := os.Stat(a); !os.IsNotExist(err) {
			t.Fatalf("o .old livre %s ficou", a)
		}
	}
	for _, a := range append(vizinhos, emUso) {
		if _, err := os.Stat(a); err != nil {
			t.Fatalf("%s não devia ter saído: %v", a, err)
		}
	}
}

// O nome do antigo: .old quando está livre; senão .old-<aleatório>, um
// diferente a cada vez.
func TestNomeDoAntigo(t *testing.T) {
	d := t.TempDir()
	para := filepath.Join(d, "arkame-agent.exe")
	if n := nomeDoAntigo(para); n != para+".old" {
		t.Fatalf("com o .old livre: %s", n)
	}
	ocupar(t, para+".old")
	a, b := nomeDoAntigo(para), nomeDoAntigo(para)
	if !strings.HasPrefix(a, para+".old-") || len(a) <= len(para+".old-") {
		t.Fatalf("com o .old ocupado: %s", a)
	}
	if a == b {
		t.Fatalf("dois nomes iguais: %s", a)
	}
}

// Depois da troca do exe no setup, os outros serviços que o rodavam (um
// segundo agente) seguiam no .old. Agora são reiniciados — menos o do
// install, que o re-registra —, e os que falham saem num aviso.
func TestReiniciarOutrosDoPrograma(t *testing.T) {
	var reiniciados []string
	reiniciar := func(n string) error {
		reiniciados = append(reiniciados, n)
		if n == "arkame-agent-falha" {
			return errors.New("acesso negado")
		}
		return nil
	}
	var out bytes.Buffer
	falharam := reiniciarOutrosDoPrograma(&out,
		[]string{"Arkame-Agent", "arkame-agent-oci", "backup-oci", "arkame-agent-falha"},
		"arkame-agent", reiniciar)

	if got := strings.Join(reiniciados, ","); got != "arkame-agent-oci,backup-oci,arkame-agent-falha" {
		t.Fatalf("reiniciados = %s (o do install não pode entrar; os outros, todos)", got)
	}
	if len(falharam) != 1 || falharam[0] != "arkame-agent-falha" {
		t.Fatalf("falharam = %v", falharam)
	}
	if !strings.Contains(out.String(), "Continuam na versão antiga: arkame-agent-falha") {
		t.Fatalf("sem o aviso dos que ficaram na versão antiga:\n%s", out.String())
	}

	out.Reset()
	reiniciados = nil
	if f := reiniciarOutrosDoPrograma(&out, []string{"arkame-agent"}, "arkame-agent", reiniciar); f != nil || reiniciados != nil || out.Len() != 0 {
		t.Fatalf("só o do install: reiniciou %v, falharam %v, saída %q", reiniciados, f, out.String())
	}
}
