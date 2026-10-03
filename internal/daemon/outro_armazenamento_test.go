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
	"sync/atomic"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Plano de um armazenamento que não é o instalado: a sessão abre e falha com
// wrong_storage, sem tocar no bucket nem rodar o comando de antes. Antes, o
// backup subia com a chave do armazenamento instalado para um bucket que a
// restauração recusa (wrong_bucket).
func TestPlanoDeOutroArmazenamentoFalhaSemEnviar(t *testing.T) {
	painel, c := novoPainel(t)
	var chamadasS3 atomic.Int32
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		chamadasS3.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(bucket.Close)
	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(bucket.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})
	cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageID: "s-instalado", StorageBucket: "acme-bkp"}
	plano := planoComArquivo(t)
	plano.StorageRef = api.StorageRef{ID: "s-outro", Bucket: "acme-arq"}
	marca := filepath.Join(t.TempDir(), "rodou")
	if runtime.GOOS != "windows" {
		plano.PreHook = "touch " + marca
	}

	err := executePlan(context.Background(), c, s3c, cfg, plano)
	if err == nil || !strings.Contains(err.Error(), "wrong_storage") {
		t.Fatalf("esperava erro wrong_storage, veio %v", err)
	}
	if n := chamadasS3.Load(); n != 0 {
		t.Fatalf("%d chamada(s) ao bucket de outro armazenamento", n)
	}
	if _, errStat := os.Stat(marca); errStat == nil {
		t.Fatal("o comando de antes rodou num plano recusado")
	}
	if n := painel.recebeu("/sessions/s1/complete"); n != 0 {
		t.Fatalf("a sessão foi concluída; chamadas: %v", painel.chamadas)
	}
	corpo := painel.corpo("/api/agents/a1/sessions/s1/fail")
	if !strings.Contains(corpo, `"error_code":"wrong_storage"`) ||
		!strings.Contains(corpo, "s-outro") || !strings.Contains(corpo, "s-instalado") {
		t.Fatalf("o /fail não diz a causa: %s", corpo)
	}
}

func TestPlanoDeOutroArmazenamento(t *testing.T) {
	cfg := &config.Config{StorageID: "s1"}
	plano := func(id string) api.Plan { return api.Plan{StorageRef: api.StorageRef{ID: id}} }
	if !planoDeOutroArmazenamento(cfg, plano("s2")) {
		t.Fatal("plano de outro armazenamento deveria ser recusado")
	}
	if planoDeOutroArmazenamento(cfg, plano("s1")) {
		t.Fatal("plano do armazenamento instalado roda")
	}
	if planoDeOutroArmazenamento(cfg, plano("")) {
		t.Fatal("plano sem id de armazenamento roda como antes")
	}
	if planoDeOutroArmazenamento(&config.Config{}, plano("s2")) {
		t.Fatal("sem STORAGE_ID (instalação antiga), nada é recusado")
	}
}
