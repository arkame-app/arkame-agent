package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type corpoDoFail struct {
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func lerFail(t *testing.T, painel *painelFalso) corpoDoFail {
	t.Helper()
	var f corpoDoFail
	if err := json.Unmarshal([]byte(painel.corpo("/api/agents/a1/sessions/s1/fail")), &f); err != nil {
		t.Fatalf("corpo do /fail: %v (chamadas: %v)", err, painel.chamadas)
	}
	return f
}

// Serviço parando durante o comando de antes (pg_dump rodando quando o
// install.sh reinicia o agente): a sessão fecha como agent_stopped, e não
// pre_hook_failed com "código -1", que mandava conferir o comando do plano.
func TestServicoParandoNoComandoDeAntesNaoCulpaOComando(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell do teste é POSIX")
	}
	painel, c := novoPainel(t)
	plan := planoComArquivo(t)
	plan.PreHook = "sleep 5"
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)
	inicio := time.Now()
	if err := executePlan(ctx, c, bucketDeTeste(t, true), cfg, plan); err == nil {
		t.Fatal("backup interrompido não pode voltar sem erro")
	}
	if time.Since(inicio) > 4*time.Second {
		t.Fatal("o comando de antes não foi derrubado com o serviço parando")
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 1 {
		t.Fatalf("/fail chamado %d vezes; chamadas: %v", n, painel.chamadas)
	}
	if n := painel.recebeu("/sessions/s1/complete"); n != 0 {
		t.Fatalf("sessão interrompida antes do envio foi concluída; chamadas: %v", painel.chamadas)
	}
	f := lerFail(t, painel)
	if f.ErrorCode != "agent_stopped" || f.ErrorMessage != "interrompido: o serviço do agente parou" {
		t.Fatalf("/fail = %+v; queria agent_stopped / interrompido: o serviço do agente parou", f)
	}
}

// Serviço parando com o envio em curso e a graça da finalização acabando
// durante o comando de depois: a nota da sessão diz que ele foi interrompido,
// não que falhou.
func TestServicoParandoNoComandoDeDepoisNaoCulpaOComando(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell do teste é POSIX")
	}
	antes := finalizacaoGraca
	finalizacaoGraca = 300 * time.Millisecond
	defer func() { finalizacaoGraca = antes }()

	painel, c := novoPainel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Na primeira chamada ao bucket, o serviço "para".
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
	plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{dir},
		StorageRef: api.StorageRef{Bucket: "b"}, PostHook: "sleep 5"}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	_ = executePlan(ctx, c, s3c, cfg, plan)

	if n := painel.recebeu("/sessions/s1/fail"); n != 1 {
		t.Fatalf("/fail chamado %d vezes; chamadas: %v", n, painel.chamadas)
	}
	f := lerFail(t, painel)
	if f.ErrorCode != "sync_failed" {
		t.Fatalf("error_code = %q; queria sync_failed", f.ErrorCode)
	}
	if !strings.HasSuffix(f.ErrorMessage, "; comando de depois interrompido: o serviço do agente parou") ||
		strings.Contains(f.ErrorMessage, "comando de depois falhou") {
		t.Fatalf("error_message culpa o comando de depois: %q", f.ErrorMessage)
	}
}
