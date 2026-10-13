package restore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketFrio responde InvalidObjectState a todo GetObject e conta os
// RestoreObject (POST ?restore) que recebe.
func bucketFrio(t *testing.T, reidratacoes *atomic.Int32) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("restore"):
			reidratacoes.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidObjectState</Code><Message>frio</Message><StorageClass>GLACIER</StorageClass></Error>`)
		default: // HeadObject: sem restauração em andamento
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
}

// O teste de restauração por amostra não pode disparar reidratação, que o
// provedor cobra: com SemReidratar, o objeto em classe fria devolve ErrFrio e
// nenhum RestoreObject sai. Sem a opção (a restauração que o cliente pediu), o
// pedido sai, como sempre.
func TestSemReidratarNaoPedeRestoreObject(t *testing.T) {
	var reidratacoes atomic.Int32
	s3c := bucketFrio(t, &reidratacoes)
	item := itemDe(t.TempDir(), "frio.bin", []byte("conteúdo"), "overwrite")

	err := Run(context.Background(), Options{S3: s3c, HostRoot: "/", SemReidratar: true}, item)
	if !errors.Is(err, ErrFrio) {
		t.Fatalf("com SemReidratar esperava ErrFrio, veio %v", err)
	}
	if n := reidratacoes.Load(); n != 0 {
		t.Fatalf("com SemReidratar saíram %d RestoreObject", n)
	}
	if nomes := nomesEm(t, item.DestPath); len(nomes) != 0 {
		t.Fatalf("ficou arquivo no destino: %v", nomes)
	}

	// O bucket de mentira conta mesmo: a restauração comum pede a reidratação.
	var pedida *ErrWarmingRequested
	err = Run(context.Background(), Options{S3: s3c, HostRoot: "/"}, item)
	if !errors.As(err, &pedida) || reidratacoes.Load() != 1 {
		t.Fatalf("sem a opção esperava um RestoreObject e ErrWarmingRequested; veio %v, %d pedido(s)", err, reidratacoes.Load())
	}
}
