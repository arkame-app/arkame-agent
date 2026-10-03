package daemon

import (
	"context"
	"testing"
	"time"
)

func recuosCurtos(t *testing.T) {
	antes := recuosDoComplete
	recuosDoComplete = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { recuosDoComplete = antes })
}

// Erro passageiro do painel no /complete jogava fora o version_map inteiro.
func TestConcluirSessaoTentaDeNovo(t *testing.T) {
	recuosCurtos(t)
	painel, c := novoPainel(t)
	painel.resposta = func(_ string, n int) (int, string) {
		if n <= 2 {
			return 503, "fora do ar"
		}
		return 0, ""
	}
	if err := concluirSessao(context.Background(), c, "/x/complete", struct{}{}, nil); err != nil {
		t.Fatalf("esperava sucesso na terceira tentativa: %v", err)
	}
	if n := painel.recebeu("/x/complete"); n != 3 {
		t.Fatalf("tentativas = %d, queria 3", n)
	}
}

// Desiste depois das tentativas, e não insiste em erro do lado do agente (4xx).
func TestConcluirSessaoDesiste(t *testing.T) {
	recuosCurtos(t)
	painel, c := novoPainel(t)
	painel.resposta = func(string, int) (int, string) { return 500, "x" }
	if err := concluirSessao(context.Background(), c, "/x/complete", struct{}{}, nil); err == nil {
		t.Fatal("esperava erro depois de esgotar as tentativas")
	}
	if n := painel.recebeu("/x/complete"); n != 5 {
		t.Fatalf("tentativas = %d, queria 5", n)
	}

	painel2, c2 := novoPainel(t)
	painel2.resposta = func(string, int) (int, string) { return 400, `{"error":"invalid stats"}` }
	if err := concluirSessao(context.Background(), c2, "/y/complete", struct{}{}, nil); err == nil {
		t.Fatal("4xx é erro")
	}
	if n := painel2.recebeu("/y/complete"); n != 1 {
		t.Fatalf("4xx não melhora tentando de novo; tentativas = %d", n)
	}
}

// Uma tentativa anterior chegou e só a resposta se perdeu: a sessão já está
// concluída, e isso é sucesso.
func TestConcluirSessaoJaConcluida(t *testing.T) {
	recuosCurtos(t)
	painel, c := novoPainel(t)
	painel.resposta = func(_ string, n int) (int, string) {
		if n == 1 {
			return 502, "gateway"
		}
		return 400, `{"error":"not_running","message":"session já está em status complete"}`
	}
	if err := concluirSessao(context.Background(), c, "/x/complete", struct{}{}, nil); err != nil {
		t.Fatalf("not_running depois de uma tentativa é sessão já concluída: %v", err)
	}
}
