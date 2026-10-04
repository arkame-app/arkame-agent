package daemon

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

// bucketQueApagaTudo responde a todo DeleteObjects como se tivesse apagado
// cada versão pedida, e conta as chamadas.
func bucketQueApagaTudo(t *testing.T, chamadas *int) *httptest.Server {
	var mu gosync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") {
			http.Error(w, "não implementado", http.StatusNotImplemented)
			return
		}
		mu.Lock()
		*chamadas++
		mu.Unlock()
		var pedido struct {
			Objects []struct {
				Key       string `xml:"Key"`
				VersionID string `xml:"VersionId"`
			} `xml:"Object"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(b, &pedido); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult>`)
		for _, o := range pedido.Objects {
			fmt.Fprintf(&sb, `<Deleted><Key>%s</Key><VersionId>%s</VersionId></Deleted>`, o.Key, o.VersionID)
		}
		sb.WriteString(`</DeleteResult>`)
		_, _ = io.WriteString(w, sb.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// O serviço para no meio da limpeza, entre dois lotes: as versões do primeiro
// já saíram do bucket, e o relato delas ainda chega ao painel. Antes, o POST
// do purge-result usava o ctx cancelado e falhava na hora; se o agente não
// voltasse, o catálogo seguia oferecendo versões que não existem mais.
func TestServicoParandoAindaRelataOExpurgo(t *testing.T) {
	const total = 1005 // dois lotes: 1000 + 5
	painel, c := novoPainel(t)
	var plano api.PurgePlanResponse
	plano.RunID = "r1"
	plano.Bucket = "b"
	for i := 0; i < total; i++ {
		plano.Versions = append(plano.Versions, api.PurgePlanVersion{
			Key: fmt.Sprintf("arkame/data/a/%05d", i), VersionID: "v1", Reason: "hard_delete"})
	}
	corpoDoPlano, _ := json.Marshal(plano)
	painel.resposta = func(path string, n int) (int, string) {
		if strings.HasSuffix(path, "/purge-plan") {
			return http.StatusOK, string(corpoDoPlano)
		}
		return 0, ""
	}

	var deletes int
	bucket := bucketQueApagaTudo(t, &deletes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var uma gosync.Once
	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(bucket.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
		// O serviço "para" quando o primeiro DeleteObjects termina, com a
		// resposta já lida: o cancelamento cai entre os dois lotes.
		APIOptions: []func(*middleware.Stack) error{func(st *middleware.Stack) error {
			return st.Initialize.Add(middleware.InitializeMiddlewareFunc("paraDepoisDoPrimeiroLote",
				func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
					out, md, err := next.HandleInitialize(ctx, in)
					if awsmiddleware.GetOperationName(ctx) == "DeleteObjects" {
						uma.Do(cancel)
					}
					return out, md, err
				}), middleware.After)
		}},
	})

	expurgar(ctx, c, s3c, cfgDaExclusao())

	if deletes != 1 {
		t.Fatalf("esperava só o primeiro lote enviado ao bucket, foram %d DeleteObjects", deletes)
	}
	if n := painel.recebeu("/purge-result"); n != 1 {
		t.Fatalf("o relato do expurgo não chegou ao painel com o serviço parando; chamadas: %v", painel.chamadas)
	}
	var relato api.PurgeResult
	if err := json.Unmarshal([]byte(painel.corpo("/api/agents/a1/purge-result")), &relato); err != nil {
		t.Fatal(err)
	}
	if relato.RunID != "r1" || len(relato.Deleted) != 1000 || len(relato.Failed) != 0 {
		t.Fatalf("esperava as 1000 versões do primeiro lote e nenhuma falha; veio run=%q deleted=%d failed=%d",
			relato.RunID, len(relato.Deleted), len(relato.Failed))
	}
	if relato.Deleted[0].Key != "arkame/data/a/00000" || relato.Deleted[999].Key != "arkame/data/a/00999" {
		t.Fatalf("o relato não traz as versões do primeiro lote: %q … %q", relato.Deleted[0].Key, relato.Deleted[999].Key)
	}
}
