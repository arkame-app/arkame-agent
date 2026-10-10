package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// bucketDeVerdade cria um bucket versionado no S3 de teste (RustFS local ou o
// da CI, em ARKAME_TEST_S3_ENDPOINT) e o apaga no fim. Sem a variável o teste
// é pulado — menos na CI, onde faltar o bucket é falha (ver internal/purge).
func bucketDeVerdade(t *testing.T) (*s3.Client, string) {
	t.Helper()
	endpoint := os.Getenv("ARKAME_TEST_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("CI") == "true" {
			t.Fatal("na CI este teste exige ARKAME_TEST_S3_ENDPOINT — o S3 local sumiu do workflow?")
		}
		t.Skip("ARKAME_TEST_S3_ENDPOINT não definido")
	}
	valor := func(k, padrao string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return padrao
	}
	c := s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider(valor("ARKAME_TEST_S3_KEY", "arkametest"), valor("ARKAME_TEST_S3_SECRET", "arkametest123"), ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	bucket := fmt.Sprintf("sync-test-%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("criando bucket: %v", err)
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  &bucket,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("ligando versionamento: %v", err)
	}
	t.Cleanup(func() {
		p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: &bucket})
		for p.HasMorePages() {
			pg, err := p.NextPage(ctx)
			if err != nil {
				return
			}
			for _, v := range pg.Versions {
				_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: v.Key, VersionId: v.VersionId})
			}
			for _, v := range pg.DeleteMarkers {
				_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: v.Key, VersionId: v.VersionId})
			}
		}
		_, _ = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
	})
	return c, bucket
}

// O engine novo contra um S3 de verdade (versionamento do provedor): envio em
// paralelo com um multipart no meio, atalho, objeto apagado por fora pego
// pela reconferência, e estado perdido sem reenvio — conferindo cada versão
// do índice com um GetObject por VersionId.
func TestEngineContraS3DeVerdade(t *testing.T) {
	c, bucket := bucketDeVerdade(t)
	antes := limiarDoMultipart
	limiarDoMultipart = 200_000
	t.Cleanup(func() { limiarDoMultipart = antes })

	m := novaMesa(t)
	m.cli = c
	m.semear(80)
	m.escrever("grande/multipart.bin", strings.Repeat("0123456789abcdef", 16_000)) // 256 KB → multipart
	m.escrever("vazio.txt", "")
	const n = 82
	ctx := context.Background()

	rodar := func() (*Result, []api.IndexEntry) {
		t.Helper()
		m.sessao++
		base, err := estado.Abrir(m.dirEst, estado.Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		defer base.Fechar()
		plano, _ := base.Plano("p1", PrefixoDasChaves(prefixoDeTeste))
		delta := !plano.Vazio()
		ex, err := plano.Comecar(fmt.Sprint(m.sessao))
		if err != nil {
			t.Fatal(err)
		}
		var em []api.IndexEntry
		r, err := Run(ctx, EngineOptions{
			S3: c, Bucket: bucket, PrefixRoot: prefixoDeTeste, HostRoot: "/",
			SourcePaths: []string{m.raiz}, Trabalhadores: 4, Estado: ex, Delta: delta,
			Emitir: func(e api.IndexEntry) error { em = append(em, e); return nil },
			Agora:  func() time.Time { return m.agora },
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range em {
			if e.Op == api.OpUpsert {
				m.presenca[e.Key] = api.FileEntry{Key: e.Key, VersionID: e.VersionID, Size: *e.Size, SHA256: e.SHA256}
			} else {
				delete(m.presenca, e.Key)
			}
		}
		if !delta {
			if len(em) != n {
				t.Fatalf("inventário completo com %d entradas", len(em))
			}
		}
		if err := plano.Confirmar(fmt.Sprint(m.sessao), r.Stats.FilesTotal, ex.Ger); err != nil {
			t.Fatal(err)
		}
		return r, em
	}
	// Cada entrada do índice aponta para uma versão que existe, com o
	// conteúdo do arquivo de agora, o metadado sha256 e a versão atual.
	conferir := func() {
		t.Helper()
		if len(m.presenca) != n {
			t.Fatalf("índice com %d arquivos, queria %d", len(m.presenca), n)
		}
		for k, e := range m.presenca {
			local, err := os.ReadFile("/" + strings.TrimPrefix(k, prefixoDeTeste))
			if err != nil {
				t.Fatal(err)
			}
			if e.VersionID == "" || e.VersionID == "null" {
				t.Fatalf("%s sem versão de verdade: %q", k, e.VersionID)
			}
			out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &k, VersionId: &e.VersionID})
			if err != nil {
				t.Fatalf("a versão do índice não existe: %s@%s: %v", k, e.VersionID, err)
			}
			remoto, _ := io.ReadAll(out.Body)
			out.Body.Close()
			if string(remoto) != string(local) || out.Metadata["sha256"] != sha256Hex(local) || e.SHA256 != sha256Hex(local) {
				t.Fatalf("%s@%s: conteúdo ou sha256 não conferem", k, e.VersionID)
			}
			head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &k})
			if err != nil || aws.ToString(head.VersionId) != e.VersionID {
				t.Fatalf("%s: o índice aponta %s, a versão atual é %s (%v)", k, e.VersionID, aws.ToString(head.VersionId), err)
			}
		}
	}

	r, _ := rodar()
	if r.Stats.FilesUploaded != n || r.FilesFailed != 0 {
		t.Fatalf("primeira: enviados=%d falhas=%d (%s)", r.Stats.FilesUploaded, r.FilesFailed, r.PrimeiraFalha)
	}
	conferir()

	r, em := rodar()
	if r.Atalhos != n || r.Stats.FilesUploaded != 0 || len(em) != 0 {
		t.Fatalf("sem mudança: atalhos=%d enviados=%d emitidas=%d", r.Atalhos, r.Stats.FilesUploaded, len(em))
	}

	// Alguém apaga uma versão direto no bucket; e um arquivo muda.
	sumido := m.chave("p05/arquivo-0005.txt")
	v := m.presenca[sumido].VersionID
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &sumido, VersionId: &v}); err != nil {
		t.Fatal(err)
	}
	m.escrever("p00/arquivo-0000.txt", "conteúdo novo")
	reenvios := int64(0)
	for dia := 1; dia <= 31; dia++ {
		m.agora = m.agora.Add(24 * time.Hour)
		r, _ := rodar()
		reenvios += r.Stats.FilesUploaded
	}
	if reenvios != 2 || m.presenca[sumido].VersionID == v {
		t.Fatalf("no ciclo sobem o alterado e o que sumiu do bucket: reenvios=%d", reenvios)
	}
	conferir()

	// Estado perdido: lê e confere tudo, reaproveita as versões que estão lá.
	antesDaPerda := map[string]string{}
	for k, e := range m.presenca {
		antesDaPerda[k] = e.VersionID
	}
	matches, _ := filepath.Glob(filepath.Join(m.dirEst, "*.db"))
	for _, f := range matches {
		_ = os.Remove(f)
	}
	r, _ = rodar()
	if r.Stats.FilesUploaded != 0 || r.Atalhos != 0 || r.Stats.FilesTotal != n {
		t.Fatalf("estado perdido: enviados=%d atalhos=%d total=%d", r.Stats.FilesUploaded, r.Atalhos, r.Stats.FilesTotal)
	}
	for k, e := range m.presenca {
		if antesDaPerda[k] != e.VersionID {
			t.Fatalf("%s mudou de versão sem mudar de conteúdo: %s → %s", k, antesDaPerda[k], e.VersionID)
		}
	}
	conferir()
}
