package purge

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Falso é um bucket versionado mínimo em memória: HeadObject (versão atual)
// e DeleteObjects por versão. Os testes contra RustFS provam a semântica do
// S3; este prova a decisão do agent sem precisar de bucket.
type s3Falso struct {
	mu       gosync.Mutex
	versoes  map[string][]string // chave → versões, a última é a atual
	heads    int
	headErro int // se != 0, HeadObject responde com este status
}

func novoS3Falso(t *testing.T) (*s3Falso, *s3.Client) {
	t.Helper()
	f := &s3Falso{versoes: map[string][]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(srv.URL),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
	return f, c
}

func (f *s3Falso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	partes := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	chave := ""
	if len(partes) == 2 {
		chave = partes[1]
	}
	switch {
	case r.Method == http.MethodHead:
		f.heads++
		if f.headErro != 0 {
			w.WriteHeader(f.headErro)
			return
		}
		vs := f.versoes[chave]
		if len(vs) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("x-amz-version-id", vs[len(vs)-1])
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
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
			vs := f.versoes[o.Key]
			for i, v := range vs {
				if v == o.VersionID {
					f.versoes[o.Key] = append(vs[:i:i], vs[i+1:]...)
					break
				}
			}
			fmt.Fprintf(&sb, `<Deleted><Key>%s</Key><VersionId>%s</VersionId></Deleted>`, o.Key, o.VersionID)
		}
		sb.WriteString(`</DeleteResult>`)
		_, _ = io.WriteString(w, sb.String())
	default:
		http.Error(w, "não implementado no s3Falso", http.StatusNotImplemented)
	}
}

func (f *s3Falso) existe(key, versao string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.versoes[key] {
		if v == versao {
			return true
		}
	}
	return false
}
