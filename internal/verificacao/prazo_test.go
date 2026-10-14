package verificacao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Revisão do S9 (M2): o HeadObject de cada item tem prazo próprio. O provedor
// que aceita a conexão e não responde não segura a rodada (e a trava do
// bucket) até o prazo dela: o item é pulado como erro do momento — nunca
// falha — e a rodada segue.
func TestConsultaSemRespostaPulaOItem(t *testing.T) {
	antes := prazoDaConsulta
	prazoDaConsulta = 80 * time.Millisecond
	t.Cleanup(func() { prazoDaConsulta = antes })

	b, _ := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	mudo := b.guardar(prefixo+"mudo", []byte("aaa"))
	bom := b.guardar(prefixo+"bom", []byte("bbb"))
	solta := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/b/"+mudo.Key {
			select {
			case <-solta:
			case <-time.After(5 * time.Second):
			}
		}
		b.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(solta) })
	s3c := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})

	ini := time.Now()
	res, interrompida := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{mudo, bom})
	if d := time.Since(ini); interrompida || d > 2*time.Second {
		t.Fatalf("a rodada ficou %s presa no HeadObject sem resposta (prazo da consulta: 80 ms; interrompida=%v)", d, interrompida)
	}
	conferir(t, res[0], StatusPulado, PuladoTransitorio)
	conferir(t, res[1], StatusOK, "")
}
