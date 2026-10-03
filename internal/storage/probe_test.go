package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketVersionado responde o mínimo do probe: versionamento ligado, sem
// Object Lock nem lifecycle, e a listagem de versões em duas páginas. A
// listagem só das atuais (ListObjectsV2) devolve um total menor, para o teste
// pegar quem a usar.
type bucketVersionado struct {
	listagemFalha bool
}

func (b *bucketVersionado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case q.Has("versioning"):
		_, _ = io.WriteString(w, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	case q.Has("object-lock"), q.Has("lifecycle"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
	case b.listagemFalha:
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>negado</Message></Error>`)
	case q.Has("versions") && q.Get("key-marker") == "":
		// a.txt: atual 100 + antiga 1000. b.txt: delete marker no topo e uma
		// versão antiga de 10.
		_, _ = io.WriteString(w, `<ListVersionsResult><Name>b</Name><IsTruncated>true</IsTruncated>
<NextKeyMarker>a.txt</NextKeyMarker><NextVersionIdMarker>a1</NextVersionIdMarker>
<Version><Key>a.txt</Key><VersionId>a2</VersionId><IsLatest>true</IsLatest><Size>100</Size></Version>
<Version><Key>a.txt</Key><VersionId>a1</VersionId><IsLatest>false</IsLatest><Size>1000</Size></Version>
</ListVersionsResult>`)
	case q.Has("versions"):
		_, _ = io.WriteString(w, `<ListVersionsResult><Name>b</Name><IsTruncated>false</IsTruncated>
<DeleteMarker><Key>b.txt</Key><VersionId>b2</VersionId><IsLatest>true</IsLatest></DeleteMarker>
<Version><Key>b.txt</Key><VersionId>b1</VersionId><IsLatest>false</IsLatest><Size>10</Size></Version>
</ListVersionsResult>`)
	case q.Get("list-type") == "2":
		_, _ = io.WriteString(w, `<ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>
<Contents><Key>a.txt</Key><Size>100</Size></Contents></ListBucketResult>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func clienteDoBucket(t *testing.T, b *bucketVersionado) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})
}

// O bucket é versionado e o provedor cobra cada versão: a ocupação soma
// todas. O ListObjectsV2 só via a atual (100 B, em vez de 1110 B).
func TestProbeSomaTodasAsVersoes(t *testing.T) {
	r := Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{}), "b", "st")
	if r.Error != "" {
		t.Fatalf("erro no probe: %s", r.Error)
	}
	if r.UsedBytes == nil || *r.UsedBytes != 1110 {
		t.Fatalf("used_bytes = %v, queria 1110 (todas as versões)", valor(r.UsedBytes))
	}
	if r.ObjectCount == nil || *r.ObjectCount != 1 {
		t.Fatalf("object_count = %v, queria 1 (só a.txt está presente)", valor(r.ObjectCount))
	}
}

// Listagem que falha não pode virar "0 B" no painel: os campos vão ausentes.
func TestProbeSemListagemNaoMandaZero(t *testing.T) {
	r := Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{listagemFalha: true}), "b", "st")
	if r.Versioning != "Enabled" {
		t.Fatalf("o resto do probe deveria seguir: %+v", r)
	}
	if r.UsedBytes != nil || r.ObjectCount != nil {
		t.Fatalf("ocupação inventada: used=%v count=%v", valor(r.UsedBytes), valor(r.ObjectCount))
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "used_bytes") || strings.Contains(string(b), "object_count") {
		t.Fatalf("o JSON leva a ocupação mesmo sem medir: %s", b)
	}
}

func valor(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}
