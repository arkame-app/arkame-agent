package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	syncengine "github.com/arkame-app/agent/internal/sync"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketComVersionamento responde ao GetBucketVersioning com o estado dado
// ("" = nunca ativado), aceita PUT devolvendo VersionId "v1" e conta os PUTs.
func bucketComVersionamento(t *testing.T, estado string, puts *atomic.Int32) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Has("versioning"):
			status := ""
			if estado != "" {
				status = "<Status>" + estado + "</Status>"
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+status+`</VersioningConfiguration>`)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut:
			puts.Add(1)
			w.Header().Set("x-amz-version-id", "v1")
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

// Versionamento suspenso ou desligado: a sessão falha antes de enviar
// qualquer arquivo. Só com a checagem depois do envio, cada execução gravava
// uma versão "null" por cima da "null" já indexada antes de abortar.
func TestVersionamentoNaoAtivoFalhaAntesDeEnviar(t *testing.T) {
	for _, tc := range []struct{ estado, causa string }{
		{"Suspended", "suspenso"},
		{"", "desligado"},
	} {
		t.Run("estado="+tc.estado, func(t *testing.T) {
			painel, c := novoPainel(t)
			cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
			var puts atomic.Int32

			err := executePlan(context.Background(), c, bucketComVersionamento(t, tc.estado, &puts), cfg, planoComArquivo(t))
			if !errors.Is(err, syncengine.ErrBucketSemVersionamento) {
				t.Fatalf("esperava ErrBucketSemVersionamento, veio %v", err)
			}
			if n := puts.Load(); n != 0 {
				t.Fatalf("%d arquivo(s) enviado(s) com o versionamento não ativo", n)
			}
			if n := painel.recebeu("/sessions/s1/complete"); n != 0 {
				t.Fatalf("a sessão foi concluída; chamadas: %v", painel.chamadas)
			}
			corpo := painel.corpo("/api/agents/a1/sessions/s1/fail")
			if !strings.Contains(corpo, "bucket_unversioned") || !strings.Contains(corpo, tc.causa) {
				t.Fatalf("o /fail não diz a causa (%s): %s", tc.causa, corpo)
			}
		})
	}
}

// Com o versionamento ativo, o backup segue normal.
func TestVersionamentoAtivoSegueOBackup(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	var puts atomic.Int32

	if err := executePlan(context.Background(), c, bucketComVersionamento(t, "Enabled", &puts), cfg, planoComArquivo(t)); err != nil {
		t.Fatal(err)
	}
	if puts.Load() != 1 {
		t.Fatalf("PUTs = %d, queria 1", puts.Load())
	}
	if painel.recebeu("/sessions/s1/complete") != 1 {
		t.Fatalf("a sessão não foi concluída; chamadas: %v", painel.chamadas)
	}
}
