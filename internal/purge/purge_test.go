package purge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Estes testes rodam contra um S3 de verdade (MinIO), porque o que precisa
// ser provado aqui não é a lógica em Go: é que a chamada apaga a versão certa
// e deixa as outras em pé. Um mock do S3 confirmaria apenas que escrevemos o
// que achamos que escrevemos.
//
// Sem ARKAME_TEST_S3_ENDPOINT os testes que precisam do bucket são pulados; a
// validação local (que é o que impede perda de dados) roda sempre.
//
//	podman run -d --rm -p 19000:9000 -e MINIO_ROOT_USER=arkametest \
//	  -e MINIO_ROOT_PASSWORD=arkametest123 quay.io/minio/minio server /data
//	ARKAME_TEST_S3_ENDPOINT=http://127.0.0.1:19000 go test ./internal/purge/...

func clienteDeTeste(t *testing.T) (*s3.Client, string) {
	t.Helper()
	endpoint := os.Getenv("ARKAME_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ARKAME_TEST_S3_ENDPOINT não definido")
	}

	usuario := valorOuPadrão("ARKAME_TEST_S3_KEY", "arkametest")
	senha := valorOuPadrão("ARKAME_TEST_S3_SECRET", "arkametest123")

	c := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(usuario, senha, ""),
	})

	bucket := fmt.Sprintf("purge-test-%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("criando bucket: %v", err)
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("ligando versionamento: %v", err)
	}
	return c, bucket
}

func valorOuPadrão(chave, padrão string) string {
	if v := os.Getenv(chave); v != "" {
		return v
	}
	return padrão
}

func subir(t *testing.T, c *s3.Client, bucket, key, conteúdo string) string {
	t.Helper()
	out, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(conteúdo)),
	})
	if err != nil {
		t.Fatalf("subindo %s: %v", key, err)
	}
	if out.VersionId == nil {
		t.Fatalf("bucket devolveu objeto sem VersionId — versionamento desligado?")
	}
	return *out.VersionId
}

func versõesDe(t *testing.T, c *s3.Client, bucket, key string) []string {
	t.Helper()
	out, err := c.ListObjectVersions(context.Background(), &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("listando versões: %v", err)
	}
	var ids []string
	for _, v := range out.Versions {
		ids = append(ids, *v.VersionId)
	}
	return ids
}

func TestRunApagaSóAVersãoAutorizada(t *testing.T) {
	c, bucket := clienteDeTeste(t)
	key := "arkame/data/agente/banco.dump"

	v1 := subir(t, c, bucket, key, "primeira")
	v2 := subir(t, c, bucket, key, "segunda")
	v3 := subir(t, c, bucket, key, "terceira")

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: bucket, PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: v2, Size: 7, Reason: "thinning"}})

	if len(failed) != 0 {
		t.Fatalf("falhas inesperadas: %+v", failed)
	}
	if len(deleted) != 1 || deleted[0].VersionID != v2 {
		t.Fatalf("esperava a versão do meio apagada, veio %+v", deleted)
	}

	restantes := versõesDe(t, c, bucket, key)
	if len(restantes) != 2 {
		t.Fatalf("esperava 2 versões restantes, achei %d", len(restantes))
	}
	for _, id := range restantes {
		if id == v2 {
			t.Fatal("a versão apagada ainda está no bucket")
		}
	}

	// O que o cliente veria: a versão atual continua baixável e íntegra.
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("versão atual ficou inacessível: %v", err)
	}
	defer out.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(out.Body); err != nil {
		t.Fatalf("lendo objeto: %v", err)
	}
	if buf.String() != "terceira" {
		t.Fatalf("conteúdo atual mudou: %q", buf.String())
	}

	// E a versão antiga que não estava no plano também.
	antiga, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(v1),
	})
	if err != nil {
		t.Fatalf("versão antiga fora do plano sumiu: %v", err)
	}
	antiga.Body.Close()
	_ = v3
}

func TestRunRecusaItemSemVersionID(t *testing.T) {
	c, bucket := clienteDeTeste(t)
	key := "arkame/data/agente/importante.conf"
	subir(t, c, bucket, key, "conteúdo")

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: bucket, PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "", Size: 8}})

	if len(deleted) != 0 {
		t.Fatal("apagou item sem version_id")
	}
	if len(failed) != 1 {
		t.Fatalf("esperava uma recusa, veio %+v", failed)
	}

	// A prova de que nada foi criado: um delete marker faria o GetObject
	// devolver 404 mesmo com o objeto "existindo" no bucket.
	if _, err := c.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	}); err != nil {
		t.Fatalf("o objeto sumiu — delete marker foi criado: %v", err)
	}
	if v := versõesDe(t, c, bucket, key); len(v) != 1 {
		t.Fatalf("esperava 1 versão intacta, achei %d", len(v))
	}
}

func TestRunRecusaKeyForaDoPrefixo(t *testing.T) {
	c, bucket := clienteDeTeste(t)
	key := "documentos-pessoais/imposto.pdf" // não é nosso
	id := subir(t, c, bucket, key, "declaração")

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: bucket, PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: id, Size: 10}})

	if len(deleted) != 0 {
		t.Fatal("apagou objeto fora do prefixo do storage")
	}
	if len(failed) != 1 {
		t.Fatalf("esperava uma recusa, veio %+v", failed)
	}
	if v := versõesDe(t, c, bucket, key); len(v) != 1 {
		t.Fatalf("o arquivo do cliente foi mexido (%d versões)", len(v))
	}
}

func TestRunSeguraQuandoAVersãoJáNãoExiste(t *testing.T) {
	c, bucket := clienteDeTeste(t)
	key := "arkame/data/agente/sumido.txt"
	id := subir(t, c, bucket, key, "algo")

	// Duas rodadas com o mesmo plano: a segunda encontra um bucket em que a
	// versão já saiu. O S3 trata como sucesso, e é o comportamento que
	// queremos — reentregar um plano depois de um reinício não pode virar
	// uma enxurrada de erros.
	plano := []Version{{Key: key, VersionID: id, Size: 4}}
	opts := Options{S3: c, Bucket: bucket, PrefixRoot: "arkame/"}

	if _, failed := Run(context.Background(), opts, plano); len(failed) != 0 {
		t.Fatalf("primeira rodada falhou: %+v", failed)
	}
	deleted, failed := Run(context.Background(), opts, plano)
	if len(failed) != 0 {
		t.Fatalf("repetir o plano gerou falha: %+v", failed)
	}
	if len(deleted) != 1 {
		t.Fatalf("esperava idempotência, veio %+v", deleted)
	}
}

func TestRunEmLoteMaiorQueOTeto(t *testing.T) {
	c, bucket := clienteDeTeste(t)

	// 1001 versões força a divisão em dois lotes — o limite do DeleteObjects
	// é 1000 e estourá-lo devolve MalformedXML, não um erro óbvio.
	const total = 1001
	plano := make([]Version, 0, total)
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("arkame/data/agente/lote/%04d.bin", i)
		id := subir(t, c, bucket, key, "x")
		plano = append(plano, Version{Key: key, VersionID: id, Size: 1})
	}

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: bucket, PrefixRoot: "arkame/"}, plano)
	if len(failed) != 0 {
		t.Fatalf("falhas no lote: %+v", failed[:min(3, len(failed))])
	}
	if len(deleted) != total {
		t.Fatalf("esperava %d apagadas, veio %d", total, len(deleted))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestValidaSemBucket(t *testing.T) {
	// A validação local não precisa de S3 e é a que impede perda de dados.
	casos := []struct {
		nome  string
		v     Version
		prefx string
		erro  bool
	}{
		{"sem version id", Version{Key: "arkame/a"}, "arkame/", true},
		{"sem key", Version{VersionID: "v1"}, "arkame/", true},
		{"fora do prefixo", Version{Key: "outro/a", VersionID: "v1"}, "arkame/", true},
		{"ok", Version{Key: "arkame/a", VersionID: "v1"}, "arkame/", false},
		{"sem prefixo configurado", Version{Key: "qualquer", VersionID: "v1"}, "", false},
	}
	for _, c := range casos {
		err := validar(Options{PrefixRoot: c.prefx}, c.v)
		if (err != nil) != c.erro {
			t.Errorf("%s: erro=%v, esperado erro=%v", c.nome, err, c.erro)
		}
	}
}
