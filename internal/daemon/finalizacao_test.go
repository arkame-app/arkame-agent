package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// painelFalso registra as chamadas do agente e responde o mínimo.
type painelFalso struct {
	mu       gosync.Mutex
	chamadas []string
	// resposta, se definida, decide o status de cada chamada a um caminho.
	resposta func(path string, n int) (int, string)
	contagem map[string]int
}

func (p *painelFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	p.mu.Lock()
	p.chamadas = append(p.chamadas, r.Method+" "+r.URL.Path)
	if p.contagem == nil {
		p.contagem = map[string]int{}
	}
	p.contagem[r.URL.Path]++
	n := p.contagem[r.URL.Path]
	p.mu.Unlock()
	if p.resposta != nil {
		if st, body := p.resposta(r.URL.Path, n); st != 0 {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	if strings.HasSuffix(r.URL.Path, "/sessions/start") {
		_, _ = io.WriteString(w, `{"session_id":"s1"}`)
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func (p *painelFalso) recebeu(sufixo string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.chamadas {
		if strings.HasSuffix(c, sufixo) {
			n++
		}
	}
	return n
}

func novoPainel(t *testing.T) (*painelFalso, *api.Client) {
	t.Helper()
	p := &painelFalso{}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	return p, c
}

// Serviço parando no meio do backup: o comando de depois ainda roda (é ele
// que limpa o dump) e o painel fica sabendo da falha — antes, os dois usavam o
// ctx cancelado: o comando era morto e a sessão ficava "running" para sempre.
func TestServicoParandoAindaFinalizaASessao(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell do teste é POSIX")
	}
	painel, c := novoPainel(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// O bucket: na primeira chamada, o serviço "para".
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bucket.Close()
	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(bucket.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	marca := filepath.Join(t.TempDir(), "limpou")
	plan := api.Plan{
		ID:          "p1",
		Kind:        "backup",
		SourcePaths: []string{dir},
		StorageRef:  api.StorageRef{Bucket: "b"},
		PostHook:    "echo ok > " + marca,
	}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	_ = executePlan(ctx, c, s3c, cfg, plan)

	if painel.recebeu("/sessions/s1/fail") != 1 {
		t.Fatalf("o painel não recebeu o /fail; chamadas: %v", painel.chamadas)
	}
	if _, err := os.Stat(marca); err != nil {
		t.Fatalf("o comando de depois não rodou com o serviço parando: %v", err)
	}
}

// O contexto de finalização sobrevive ao cancelamento, mas só pela graça.
func TestContextoDeFinalizacao(t *testing.T) {
	antes := finalizacaoGraca
	finalizacaoGraca = 50 * time.Millisecond
	defer func() { finalizacaoGraca = antes }()

	ctx, cancel := context.WithCancel(context.Background())
	fctx, fcancel := contextoDeFinalizacao(ctx)
	defer fcancel()
	cancel()
	if fctx.Err() != nil {
		t.Fatal("a finalização não pode morrer junto com o serviço")
	}
	select {
	case <-fctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("passada a graça, a finalização tem de acabar")
	}
}
