//go:build linux

package service

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// envFilhoDoPrograma faz o binário de teste virar o "agente que segue rodando"
// do teste abaixo: espera uma linha na entrada e responde com o que o
// ProgramaAtual e o os.Executable dizem naquele momento.
const envFilhoDoPrograma = "ARKAME_TESTE_FILHO_DO_PROGRAMA"

func TestMain(m *testing.M) {
	if os.Getenv(envFilhoDoPrograma) == "1" {
		fmt.Println("pronto")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Println(ProgramaAtual())
		fmt.Println(lerProgramaAtual())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Dois agentes dividem o programa. Um deles troca o programa pelo caminho do
// renomeia-e-renomeia (sistema de arquivos sem link; é também o do Windows): o
// arquivo que o vizinho executa passa a se chamar `arkame-agent.anterior`, e o
// /proc/self/exe do vizinho segue o arquivo. O ProgramaAtual do vizinho tem de
// continuar sendo o caminho do programa — é o que vai ao painel
// (program_path) e o que acha os outros serviços do mesmo programa.
func TestProgramaAtualNaoMudaQuandoOVizinhoTrocaOPrograma(t *testing.T) {
	eu, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	programa := filepath.Join(dir, "arkame-agent")
	copiar(t, eu, programa)

	filho := exec.Command(programa)
	filho.Env = append(os.Environ(), envFilhoDoPrograma+"=1")
	entrada, err := filho.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	saida, err := filho.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := filho.Start(); err != nil {
		t.Skipf("não consegui executar a cópia do binário de teste (pasta temporária noexec?): %v", err)
	}
	defer func() { _ = filho.Process.Kill(); _ = filho.Wait() }()
	linhas := bufio.NewReader(saida)
	if l, _ := linhas.ReadString('\n'); strings.TrimSpace(l) != "pronto" {
		t.Fatalf("o filho não subiu: %q", l)
	}

	// A troca do vizinho: o atual vira o anterior, e um programa novo entra.
	if err := os.Rename(programa, programa+".anterior"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(programa, []byte("programa novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entrada, "\n"); err != nil {
		t.Fatal(err)
	}
	doArranque, _ := linhas.ReadString('\n')
	deAgora, _ := linhas.ReadString('\n')
	doArranque, deAgora = strings.TrimSpace(doArranque), strings.TrimSpace(deAgora)

	// A premissa (o defeito que a revisão suspeitou): lido agora, o caminho
	// segue o arquivo renomeado.
	if deAgora != programa+".anterior" {
		t.Logf("neste sistema o os.Executable não seguiu o rename (%q); o teste só cobre o ProgramaAtual", deAgora)
	}
	if doArranque != programa {
		t.Fatalf("o ProgramaAtual do vizinho virou %q depois da troca; queria %q", doArranque, programa)
	}
}

func copiar(t *testing.T, de, para string) {
	t.Helper()
	origem, err := os.Open(de)
	if err != nil {
		t.Fatal(err)
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
