package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Falso é um S3 mínimo em memória, só com o que o engine usa: HeadObject,
// PutObject, multipart e DeleteObject. Serve para provar o que chega ao
// bucket — o teste de verdade contra RustFS fica em internal/purge.
type s3Falso struct {
	mu        gosync.Mutex
	objetos   map[string]objetoFalso // chave → versão atual
	partes    map[string]map[int][]byte
	versao    int
	apagados  []string // "chave@versao"
	abortados int
	completos int
	// antes roda no começo de cada requisição (para cancelar o ctx, etc.).
	antes func(r *http.Request)
	// falhar, se devolver true, responde 500 à requisição.
	falhar func(r *http.Request) bool
	// semVersao simula bucket sem versionamento: nenhuma resposta traz
	// x-amz-version-id.
	semVersao bool
	// suspenso simula versionamento suspenso: o envio grava a versão "null".
	suspenso bool
	// ultimaModificacao, se definida, vai como Last-Modified no HeadObject.
	ultimaModificacao time.Time
}

type objetoFalso struct {
	dados  []byte
	sha256 string // metadado
	versao string
}

func novoS3Falso(t *testing.T) (*s3Falso, *s3.Client) {
	t.Helper()
	f := &s3Falso{objetos: map[string]objetoFalso{}, partes: map[string]map[int][]byte{}}
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
	if f.antes != nil {
		f.antes(r)
	}
	if f.falhar != nil && f.falhar(r) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "<Error><Code>InternalError</Code></Error>", http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// /bucket/chave/…
	partes := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	chave := ""
	if len(partes) == 2 {
		chave = partes[1]
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead:
		o, ok := f.objetos[chave]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("x-amz-meta-sha256", o.sha256)
		if !f.semVersao {
			w.Header().Set("x-amz-version-id", o.versao)
		}
		if !f.ultimaModificacao.IsZero() {
			w.Header().Set("Last-Modified", f.ultimaModificacao.UTC().Format(http.TimeFormat))
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.dados)))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		b, _ := io.ReadAll(r.Body)
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.partes[q.Get("uploadId")][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.versao++
		v := fmt.Sprintf("v%d", f.versao)
		if f.suspenso {
			v = "null"
		}
		f.objetos[chave] = objetoFalso{dados: b, sha256: r.Header.Get("x-amz-meta-sha256"), versao: v}
		if !f.semVersao {
			w.Header().Set("x-amz-version-id", v)
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("up%d", len(f.partes)+1)
		f.partes[id] = map[int][]byte{}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, chave, id)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		_, _ = io.Copy(io.Discard, r.Body)
		ps := f.partes[q.Get("uploadId")]
		var nums []int
		for n := range ps {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var dados []byte
		for _, n := range nums {
			dados = append(dados, ps[n]...)
		}
		f.versao++
		v := fmt.Sprintf("v%d", f.versao)
		if f.suspenso {
			v = "null"
		}
		f.objetos[chave] = objetoFalso{dados: dados, versao: v}
		f.completos++
		if !f.semVersao {
			w.Header().Set("x-amz-version-id", v)
		}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`, chave)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(f.partes, q.Get("uploadId"))
		f.abortados++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		f.apagados = append(f.apagados, chave+"@"+q.Get("versionId"))
		if o, ok := f.objetos[chave]; ok && (q.Get("versionId") == "" || o.versao == q.Get("versionId")) {
			delete(f.objetos, chave)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "não implementado no s3Falso", http.StatusNotImplemented)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
