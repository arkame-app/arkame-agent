package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	syncengine "github.com/arkame-app/agent/internal/sync"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketDeTeste aceita PUT e responde 404 ao HEAD (nada para deduplicar).
// Com versionado=false, nenhuma resposta traz x-amz-version-id.
func bucketDeTeste(t *testing.T, versionado bool) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			if versionado {
				w.Header().Set("x-amz-version-id", "v1")
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(srv.URL),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
}

func planoComArquivo(t *testing.T) api.Plan {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{dir},
		StorageRef: api.StorageRef{Bucket: "b"}}
}

// Bucket sem versionamento: o PUT não traz VersionId, o painel descartava
// todas as entradas e a sessão ficava "complete" com zero arquivos indexados.
func TestBucketSemVersionamentoFalhaASessao(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	err := executePlan(context.Background(), c, bucketDeTeste(t, false), cfg, planoComArquivo(t))
	if !errors.Is(err, syncengine.ErrBucketSemVersionamento) {
		t.Fatalf("esperava ErrBucketSemVersionamento, veio %v", err)
	}
	if n := painel.recebeu("/sessions/s1/complete"); n != 0 {
		t.Fatalf("a sessão foi concluída sem nada indexável; chamadas: %v", painel.chamadas)
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 1 {
		t.Fatalf("/fail chamado %d vezes; chamadas: %v", n, painel.chamadas)
	}
	corpo := painel.corpo("/api/agents/a1/sessions/s1/fail")
	if !strings.Contains(corpo, "bucket_unversioned") || !strings.Contains(corpo, "ative o versionamento") {
		t.Fatalf("o /fail não diz a causa: %s", corpo)
	}
}

// O painel indexou menos do que recebeu: o agente registra como erro.
func TestPainelIndexouMenosLogaErro(t *testing.T) {
	var buf bytes.Buffer
	antes := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(antes) })

	painel, c := novoPainel(t)
	painel.resposta = func(path string, _ int) (int, string) {
		if strings.HasSuffix(path, "/complete") {
			return 200, `{"ok":true,"files_indexed":0}`
		}
		return 0, ""
	}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, planoComArquivo(t)); err != nil {
		t.Fatalf("a sessão deveria seguir concluída: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "indexou menos") {
		t.Fatalf("esperava slog.Error do files_indexed menor; log:\n%s", log)
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 0 {
		t.Fatal("a sessão concluída não deve virar falha")
	}
}
