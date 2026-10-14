package purge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Revisão do S9 (M2): o HeadObject da versão atual tem prazo próprio. O
// provedor que aceita a conexão e não responde não segura a limpeza (e a trava
// do bucket) indefinidamente — e sem a conferência o desbaste é recusado.
func TestConferenciaDaVersaoAtualTemPrazo(t *testing.T) {
	antes := prazoDaConsulta
	prazoDaConsulta = 80 * time.Millisecond
	t.Cleanup(func() { prazoDaConsulta = antes })

	f, _ := novoS3Falso(t)
	key := "arkame/data/agente/banco.dump"
	f.versoes[key] = []string{"v1", "v2"}
	solta := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			select {
			case <-solta:
			case <-time.After(5 * time.Second):
			}
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(solta) })
	c := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})

	ini := time.Now()
	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "v1", Reason: "thinning"}})
	if d := time.Since(ini); d > 2*time.Second {
		t.Fatalf("a limpeza ficou %s presa no HeadObject sem resposta (prazo da consulta: 80 ms)", d)
	}
	if len(deleted) != 0 || !f.existe(key, "v1") {
		t.Fatalf("sem conferir a versão atual nada é apagado: %+v", deleted)
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Error, "não consegui conferir a versão atual") {
		t.Fatalf("esperava o desbaste recusado por falta da conferência, veio %+v", failed)
	}
}
