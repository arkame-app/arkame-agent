package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

// 410 (agente arquivado) é definitivo: não adianta tentar de novo.
func TestConcluirSessaoGoneNaoTentaDeNovo(t *testing.T) {
	recuosCurtos(t)
	painel, c := novoPainel(t)
	painel.resposta = func(string, int) (int, string) { return 410, "archived" }
	if err := concluirSessao(context.Background(), c, "/x/complete", struct{}{}, nil); !errors.Is(err, api.ErrGone) {
		t.Fatalf("esperava ErrGone, veio %v", err)
	}
	if n := painel.recebeu("/x/complete"); n != 1 {
		t.Fatalf("410 não melhora tentando de novo; tentativas = %d", n)
	}
}

// O /fail depois de um /complete malsucedido só sai quando o painel recusou de
// vez (4xx). Em prazo/rede/5xx o /complete pode ter sido gravado e só a
// resposta se perdeu — um /fail sobrescreveria o "complete".
func TestFalhaDoCompleteSoMarcaFalhaEmRecusaDefinitiva(t *testing.T) {
	recuosCurtos(t)
	casos := []struct {
		nome     string
		status   int
		querFail int
		querComp int
	}{
		{"5xx esgotando tentativas", 503, 0, 5},
		{"429 esgotando tentativas", 429, 0, 5},
		{"recusa 400", 400, 1, 1},
		{"agente arquivado 410", 410, 0, 1},
	}
	for _, cc := range casos {
		t.Run(cc.nome, func(t *testing.T) {
			painel, c := novoPainel(t)
			painel.resposta = func(path string, _ int) (int, string) {
				if strings.HasSuffix(path, "/complete") {
					return cc.status, `{"error":"x"}`
				}
				return 0, ""
			}
			plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{t.TempDir()},
				StorageRef: api.StorageRef{Bucket: "b"}}
			cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
			if err := executePlan(context.Background(), c, s3SemUso(t), cfg, plan); err == nil {
				t.Fatal("esperava erro do /complete")
			}
			if n := painel.recebeu("/sessions/s1/complete"); n != cc.querComp {
				t.Fatalf("/complete chamado %d vezes, queria %d; chamadas: %v", n, cc.querComp, painel.chamadas)
			}
			if n := painel.recebeu("/sessions/s1/fail"); n != cc.querFail {
				t.Fatalf("/fail chamado %d vezes, queria %d; chamadas: %v", n, cc.querFail, painel.chamadas)
			}
		})
	}
}

// Prazo estourado no /complete (o painel pode ter gravado): sem /fail.
func TestFalhaDoCompletePorPrazoNaoMarcaFalha(t *testing.T) {
	recuosCurtos(t)
	antes := prazoDoComplete
	prazoDoComplete = 30 * time.Millisecond
	t.Cleanup(func() { prazoDoComplete = antes })

	painel, c := novoPainel(t)
	painel.resposta = func(path string, _ int) (int, string) {
		if strings.HasSuffix(path, "/complete") {
			time.Sleep(200 * time.Millisecond)
		}
		return 0, ""
	}
	plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{t.TempDir()},
		StorageRef: api.StorageRef{Bucket: "b"}}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, s3SemUso(t), cfg, plan); err == nil {
		t.Fatal("esperava erro de prazo no /complete")
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 0 {
		t.Fatalf("/fail depois de prazo estourado pode sobrescrever um complete; chamadas: %v", painel.chamadas)
	}
}

// s3SemUso é um cliente S3 apontado para um servidor que responde 500: o plano
// de pasta vazia não deveria chegar a usá-lo.
func s3SemUso(t *testing.T) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})
}
