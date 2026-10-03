package hooks

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestComandoVazioNaoRoda(t *testing.T) {
	r, err := Run(context.Background(), "   ", time.Second)
	if err != nil {
		t.Fatalf("comando vazio não devia dar erro: %v", err)
	}
	if r.Ran {
		t.Fatal("comando vazio não devia rodar")
	}
}

func TestSucessoDevolveSaida(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell específico do unix")
	}
	r, err := Run(context.Background(), "echo ola", 5*time.Second)
	if err != nil {
		t.Fatalf("não devia falhar: %v", err)
	}
	if !r.Ran || r.ExitCode != 0 {
		t.Fatalf("esperava sucesso, veio %+v", r)
	}
	if !strings.Contains(r.Output, "ola") {
		t.Fatalf("saída perdida: %q", r.Output)
	}
}

func TestFalhaTrazCodigoESaida(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell específico do unix")
	}
	// O erro precisa chegar ao painel: é a única pista que o cliente terá.
	r, err := Run(context.Background(), "echo problema >&2; exit 3", 5*time.Second)
	if err == nil {
		t.Fatal("esperava erro")
	}
	if r.ExitCode != 3 {
		t.Fatalf("esperava código 3, veio %d", r.ExitCode)
	}
	if !strings.Contains(r.Output, "problema") {
		t.Fatalf("stderr não chegou: %q", r.Output)
	}
}

func TestPrazoInterrompe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell específico do unix")
	}
	inicio := time.Now()
	r, err := Run(context.Background(), "sleep 5", 300*time.Millisecond)
	if err == nil {
		t.Fatal("esperava erro de prazo")
	}
	if !r.TimedOut {
		t.Fatal("devia marcar que estourou o prazo — é outro problema que 'falhou'")
	}
	if time.Since(inicio) > 3*time.Second {
		t.Fatal("não interrompeu: o agendamento inteiro ficaria preso")
	}
}

func TestSaidaGrandeEhTruncada(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell específico do unix")
	}
	r, _ := Run(context.Background(), "head -c 40000 /dev/zero | tr '\\0' 'a'", 10*time.Second)
	if len(r.Output) > MaxOutputBytes+64 {
		t.Fatalf("saída não foi truncada: %d bytes", len(r.Output))
	}
	if !strings.HasSuffix(r.Output, "(saída truncada)") {
		t.Fatal("truncou sem avisar — quem lê acharia que o comando parou ali")
	}
}

// A saída é cortada ao chegar, não depois: um comando verboso não pode encher
// a memória do agente para no fim mandar 8 KB.
func TestBufferNaoCresceAlemDoTeto(t *testing.T) {
	b := &bufferSeguro{}
	pedaco := make([]byte, 64*1024)
	for i := 0; i < 64; i++ { // 4 MB
		if n, err := b.Write(pedaco); n != len(pedaco) || err != nil {
			t.Fatalf("o comando não pode ver erro de escrita: n=%d err=%v", n, err)
		}
	}
	if b.buf.Len() > MaxOutputBytes+1 {
		t.Fatalf("buffer com %d bytes, teto %d", b.buf.Len(), MaxOutputBytes+1)
	}
	if got := truncar(b.String()); !strings.HasSuffix(got, "(saída truncada)") {
		t.Fatal("a saída cortada tem de dizer que foi truncada")
	}
}
