package restore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketContado serve o conteúdo e conta os GetObject.
func bucketContado(t *testing.T, conteudo []byte, gets *atomic.Int32) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		_, _ = w.Write(conteudo)
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

func nomesEm(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	sort.Strings(n)
	return n
}

// O PATCH final falhava depois de uma gravação bem-sucedida, o item voltava
// na fila e o resolveConflict via o arquivo que a própria restauração acabara
// de gravar: em suffix-version, saía uma segunda cópia (app.vv1.1.conf).
func TestItemRefeitoNaoDuplicaOArquivo(t *testing.T) {
	conteudo := []byte("conteúdo do backup")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte("versão atual, diferente"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	opts := Options{S3: bucketContado(t, conteudo, &gets), HostRoot: "/"}
	item := itemDe(dir, "app.conf", conteudo, "suffix-version")

	if err := Run(context.Background(), opts, item); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), opts, item); err != nil { // o item refeito
		t.Fatal(err)
	}
	if got := nomesEm(t, dir); len(got) != 2 || got[0] != "app.conf" || got[1] != "app.vv1.conf" {
		t.Fatalf("arquivos no destino: %v; queria app.conf e app.vv1.conf", got)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("%d downloads; o item refeito não deveria baixar de novo", n)
	}

	// Destino com conteúdo diferente no nome com sufixo: restaura de novo.
	if err := os.WriteFile(filepath.Join(dir, "app.vv1.conf"), []byte("outra coisa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), opts, item); err != nil {
		t.Fatal(err)
	}
	if got := nomesEm(t, dir); len(got) != 3 || !slices.Contains(got, "app.vv1.1.conf") {
		t.Fatalf("com conteúdo diferente deveria gravar app.vv1.1.conf: %v", got)
	}

	// overwrite: o arquivo já inteiro no destino não é baixado nem regravado.
	dir2 := t.TempDir()
	item2 := itemDe(dir2, "b.bin", conteudo, "overwrite")
	if err := Run(context.Background(), Options{S3: opts.S3, HostRoot: "/"}, item2); err != nil {
		t.Fatal(err)
	}
	antes := gets.Load()
	if err := Run(context.Background(), Options{S3: opts.S3, HostRoot: "/"}, item2); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != antes {
		t.Fatal("overwrite com o arquivo já inteiro baixou de novo")
	}
}
