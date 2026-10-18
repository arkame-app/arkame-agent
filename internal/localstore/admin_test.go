package localstore

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Contra um RustFS de verdade (o contêiner de desenvolvimento): a chave da
// rede grava e lê, e não apaga versão, não cria marcador de exclusão, não
// desliga o versionamento nem apaga o bucket.
//
//	ARKAME_TESTE_RUSTFS_URL=http://127.0.0.1:19000 ARKAME_TESTE_RUSTFS_CHAVE=... ARKAME_TESTE_RUSTFS_SEGREDO=... go test ./internal/localstore -run Rede
func TestChaveDaRedeNaoApagaNoRustFSReal(t *testing.T) {
	url, ak, sk := os.Getenv("ARKAME_TESTE_RUSTFS_URL"), os.Getenv("ARKAME_TESTE_RUSTFS_CHAVE"), os.Getenv("ARKAME_TESTE_RUSTFS_SEGREDO")
	if url == "" || ak == "" || sk == "" {
		t.Skip("sem RustFS de teste (ARKAME_TESTE_RUSTFS_URL)")
	}
	ctx := context.Background()
	root := cliente(url, ak, sk)
	bucket := "teste-rede-" + strings.ToLower(time.Now().Format("150405"))
	if _, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	if err := PrepararBucket(ctx, root, bucket); err != nil {
		t.Fatal(err)
	}
	a := &Admin{Endereco: url, Chave: ak, Segredo: sk, Cliente: &http.Client{Timeout: 10 * time.Second}}
	usuario := "arkame-teste-" + strings.ToLower(time.Now().Format("150405"))
	segredo := "segredo-de-teste-da-rede-123"
	agente := "0b6f0c1e-5f1a-4f6e-9d1e-2a7c3e9b8d41"
	minha := "arkame/data/" + agente + "/"
	if err := a.DarAcessoDaRede(ctx, bucket, "arkame/", agente, usuario, segredo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.RemoverUsuario(ctx, usuario) }()
	if us, err := a.Usuarios(ctx); err != nil || !contem(us, usuario) {
		t.Fatalf("o usuário não aparece na lista: %v %v", us, err)
	}
	c := cliente(url, usuario, segredo)
	// O que é de outro servidor no mesmo bucket (o dono, outro cliente).
	alheia := "arkame/data/outro-servidor/segredo.txt"
	if _, err := root.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String(alheia), Body: bytes.NewReader([]byte("do outro"))}); err != nil {
		t.Fatal(err)
	}
	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String(minha + "x.txt"), Body: bytes.NewReader([]byte("oi"))})
	if err != nil {
		t.Fatalf("a chave da rede não grava: %v", err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String(minha + "x.txt"), VersionId: put.VersionId}); err != nil {
		t.Fatalf("a chave da rede não lê a versão: %v", err)
	}
	if _, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: aws.String(minha)}); err != nil {
		t.Fatalf("a chave da rede não lista as versões da pasta dela: %v", err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String(alheia)}); err == nil {
		t.Fatal("a chave da rede leu o backup de outro servidor")
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String(alheia), Body: bytes.NewReader([]byte("estrago"))}); err == nil {
		t.Fatal("a chave da rede gravou por cima do backup de outro servidor")
	}
	if _, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: aws.String("arkame/data/")}); err == nil {
		t.Fatal("a chave da rede listou as pastas dos outros servidores")
	}
	if _, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket}); err == nil {
		t.Fatal("a chave da rede listou o bucket inteiro")
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: aws.String(minha + "x.txt"), VersionId: put.VersionId}); err == nil {
		t.Fatal("a chave da rede apagou uma versão")
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: aws.String(minha + "x.txt")}); err == nil {
		t.Fatal("a chave da rede criou um marcador de exclusão")
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}}); err == nil {
		t.Fatal("a chave da rede desligou o versionamento")
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); err == nil {
		t.Fatal("a chave da rede apagou o bucket")
	}
	outra := &Admin{Endereco: url, Chave: usuario, Segredo: segredo, Cliente: a.Cliente}
	if err := outra.DarAcessoDaRede(ctx, bucket, "arkame/", agente, "arkame-intruso-1", "segredo-do-intruso-123"); err == nil {
		t.Fatal("a chave da rede criou outro usuário")
	}
	if err := a.RemoverUsuario(ctx, usuario); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String(minha + "y.txt"), Body: bytes.NewReader([]byte("oi"))}); err == nil {
		t.Fatal("a chave revogada ainda grava")
	}
}

func cliente(url, ak, sk string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(url),
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk}, nil
		}),
	})
}

func contem(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestNomeDeUsuarioRecusado(t *testing.T) {
	a := &Admin{}
	for _, u := range []string{"", "A", "ab", "com espaço", "../x", "arkame/x"} {
		if err := a.DarAcessoDaRede(context.Background(), "b", "arkame/", "ag-1", u, "1234567890123456"); err == nil {
			t.Fatalf("aceitou o usuário %q", u)
		}
	}
	if err := a.DarAcessoDaRede(context.Background(), "b", "arkame/", "ag-1", "arkame-x-1", "curto"); err == nil {
		t.Fatal("aceitou segredo curto")
	}
	if err := a.DarAcessoDaRede(context.Background(), "b", "arkame/", "../outro", "arkame-x-1", "1234567890123456"); err == nil {
		t.Fatal("aceitou um id de agente que sai da pasta")
	}
}
