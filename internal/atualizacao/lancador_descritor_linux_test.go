//go:build linux

package atualizacao

import (
	"crypto/ed25519"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// envFilhoDoLancador faz o binário de teste virar o lançador do teste
// TestExecutarAbertoExecutaOArquivoAberto: abre o programa, deixa o caminho
// ser trocado e executa pelo descritor.
const envFilhoDoLancador = "ARKAME_TESTE_FILHO_DO_LANCADOR"

func TestMain(m *testing.M) {
	if programa := os.Getenv(envFilhoDoLancador); programa != "" {
		f, err := os.Open(programa)
		if err != nil {
			os.Exit(90)
		}
		// A troca entre conferir e executar: outro programa entra no caminho.
		if err := os.Rename(programa+".invasor", programa); err != nil {
			os.Exit(91)
		}
		_ = ExecutarAberto(f, programa, []string{"conferido"}, os.Environ())
		os.Exit(92)
	}
	os.Exit(m.Run())
}

// O programa do volume trocado entre a conferência e a execução: o lançador
// tem nas mãos o arquivo que conferiu, e não o que está agora no caminho.
func TestLancadorEntregaOArquivoQueConferiu(t *testing.T) {
	b := noVolume(t)
	antes := aposConferirDoVolume
	t.Cleanup(func() { aposConferirDoVolume = antes })
	invasor := []byte("#!/bin/sh\necho invasor\n")
	aposConferirDoVolume = func() {
		tmp := b.lugar.Atual + ".tmp"
		if err := os.WriteFile(tmp, invasor, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, b.lugar.Atual); err != nil {
			t.Fatal(err)
		}
	}
	aberto, porque := AbrirDoVolume(b.lugar.Atual, "0.4.31", []ed25519.PublicKey{b.pub}, NomeNoRelease())
	if aberto == nil {
		t.Fatalf("não abriu o programa do volume: %s", porque)
	}
	defer aberto.Close()
	// A premissa: o caminho em texto já é o invasor.
	if c, _ := os.ReadFile(b.lugar.Atual); string(c) != string(invasor) {
		t.Fatalf("premissa: o caminho não foi trocado: %q", c)
	}
	// O que o lançador executa é o caminho do descritor.
	c, err := os.ReadFile(CaminhoDoAberto(aberto))
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != string(b.rel.programa) {
		t.Fatalf("o que seria executado não é o programa conferido:\n%s", c)
	}
}

// Um link no lugar do programa do volume não é seguido: abandonado.
func TestLancadorNaoSegueLinkNoLugarDoPrograma(t *testing.T) {
	b := noVolume(t)
	copia := filepath.Join(t.TempDir(), "copia")
	if err := os.WriteFile(copia, b.rel.programa, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.lugar.Atual); err != nil {
		t.Fatal(err)
	}
	// O alvo tem o conteúdo certo: o SHA-256 lido pelo link conferiria.
	if err := os.Symlink(copia, b.lugar.Atual); err != nil {
		t.Fatal(err)
	}
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador seguiu um link no lugar do programa do volume")
	}
	if e := b.estado(); e.Situacao != SituacaoAbandonada || e.Motivo != MotivoInvalido {
		t.Fatalf("registro = %+v", e)
	}
}

// De ponta a ponta, com programas de verdade: um processo abre o programa, o
// caminho é trocado por outro programa, e o ExecutarAberto roda o que foi
// aberto.
func TestExecutarAbertoExecutaOArquivoAberto(t *testing.T) {
	eco, err1 := exec.LookPath("echo")
	falso, err2 := exec.LookPath("false")
	eu, err3 := os.Executable()
	if err1 != nil || err2 != nil || err3 != nil {
		t.Skip("sem echo/false nesta máquina")
	}
	dir := t.TempDir()
	programa := filepath.Join(dir, "arkame-agent")
	copiarPrograma(t, eco, programa)
	copiarPrograma(t, falso, programa+".invasor")

	filho := exec.Command(eu)
	filho.Env = append(os.Environ(), envFilhoDoLancador+"="+programa)
	saida, err := filho.CombinedOutput()
	if err != nil && strings.Contains(err.Error(), "permission denied") {
		t.Skipf("pasta temporária sem execução: %v", err)
	}
	if err != nil || strings.TrimSpace(string(saida)) != "conferido" {
		t.Fatalf("o lançador não executou o programa que abriu (queria a saída do echo): %v, saída %q", err, saida)
	}
}

func copiarPrograma(t *testing.T, de, para string) {
	t.Helper()
	origem, err := os.Open(de)
	if err != nil {
		t.Skip(err)
	}
	defer origem.Close()
	destino, err := os.OpenFile(para, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destino, origem); err != nil {
		t.Fatal(err)
	}
	if err := destino.Close(); err != nil {
		t.Fatal(err)
	}
}
