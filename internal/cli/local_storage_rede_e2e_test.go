package cli

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/localstore"
	"github.com/arkame-app/agent/internal/storage"
)

// A cópia local recebendo backup de outro servidor da rede, de ponta a ponta,
// com o RustFS de verdade: install, share, join num segundo "servidor",
// gravação com a chave da rede, recusa de apagar e revogação.
//
//	ARKAME_TESTE_RUSTFS=~/.cache/arkame-rustfs/rustfs go test ./internal/cli -run Rede -v
func TestCopiaLocalNaRedeDePontaAPonta(t *testing.T) {
	programa := os.Getenv("ARKAME_TESTE_RUSTFS")
	if programa == "" {
		t.Skip("sem o programa do RustFS (ARKAME_TESTE_RUSTFS)")
	}
	antesTemp := pastasTemporarias
	pastasTemporarias = nil
	t.Cleanup(func() { pastasTemporarias = antesTemp })
	ctx := context.Background()
	base := filepath.Join(os.Getenv("HOME"), ".cache", "arkame-rede-e2e", fmt.Sprint(time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dono, cliente := filepath.Join(base, "dono"), filepath.Join(base, "cliente")
	for _, d := range []string{dono, cliente} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	escrever := func(caminho, conteudo string) {
		if err := os.WriteFile(caminho, []byte(conteudo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	escrever(filepath.Join(dono, "agent.env"), "PANEL_URL=https://painel.invalid\nAGENT_ID=ag-dono\nSTORAGE_BUCKET=nuvem-dono\nSTORAGE_ID=00000000-0000-4000-8000-000000000001\n")
	escrever(filepath.Join(cliente, "agent.env"), "PANEL_URL=https://painel.invalid\nAGENT_ID=7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f\nSTORAGE_BUCKET=nuvem-cliente\nSTORAGE_ID=00000000-0000-4000-8000-000000000002\n")
	porta := 19500 + rand.Intn(400)
	o := opcoesDaCopiaLocal{
		storageID: "0b6f0c1e-5f1a-4f6e-9d1e-2a7c3e9b8d41", bucket: "arkame-local-e2e1", pasta: filepath.Join(base, "dados"), porta: porta,
		config: filepath.Join(dono, "agent.env"), servico: "arkame-agent-local", semServico: true, programaPronto: programa,
	}
	var saida bytes.Buffer
	if err := instalarCopiaLocal(ctx, o, &saida); err != nil {
		t.Fatalf("install: %v\n%s", err, saida.String())
	}
	localCfg := filepath.Join(dono, "agent-local.env")
	// O "serviço" da cópia local: o supervisor, como o daemon o sobe.
	var parar context.CancelFunc
	subir := func() {
		cfg, err := config.Load(localCfg, config.Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		c, cancelar := context.WithCancel(ctx)
		parar = cancelar
		sup := &localstore.Supervisor{Programa: cfg.LocalStoreProgram, Dados: cfg.LocalStoreData, Endereco: cfg.LocalStoreAddress, Escuta: cfg.LocalStoreListen,
			ArquivoDaChave: cfg.LocalStoreAccessFile, ArquivoDoSegredo: cfg.LocalStoreSecretFile, PastaTLS: cfg.LocalStoreTLS, SHA256: cfg.LocalStoreProgramSHA, Log: cfg.LocalStoreLog}
		go sup.Rodar(c)
		if err := sup.Pronto(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	subir()
	t.Cleanup(func() { parar() })
	antesR, antesE := reiniciarServicoDaCopia, esperarCopiaLocal
	reiniciarServicoDaCopia = func(context.Context, string) error {
		parar()
		time.Sleep(2 * time.Second)
		subir()
		return nil
	}
	esperarCopiaLocal = func(context.Context, *config.Config) error { return nil }
	t.Cleanup(func() { reiniciarServicoDaCopia, esperarCopiaLocal = antesR, antesE })

	// share: escuta na rede, e a chave do cliente.
	saida.Reset()
	if err := compartilharCopiaLocal(ctx, opcoesDoShare{cfgPath: filepath.Join(dono, "agent.env"), servico: "arkame-agent-local", nomes: []string{"127.0.0.1"}, cliente: "ERP 01", agentID: "7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"}, &saida); err != nil {
		t.Fatalf("share: %v\n%s", err, saida.String())
	}
	texto := saida.String()
	achar := func(re string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(texto)
		if m == nil {
			t.Fatalf("a saída do share não tem %s:\n%s", re, texto)
		}
		return m[1]
	}
	impressao := achar(`--fingerprint ([0-9a-f]{64})`)
	endpoint := achar(`--endpoint (https://\S+)`)
	chave := achar(`Chave de acesso: (\S+)`)
	segredo := achar(`Segredo:\s+(\S+)`)
	if !strings.HasPrefix(chave, "arkame-erp-01-") {
		t.Fatalf("nome da chave: %s", chave)
	}
	if c, _ := config.Load(localCfg, config.Overrides{}); c.LocalStoreListen != fmt.Sprintf("0.0.0.0:%d", porta) {
		t.Fatalf("a cópia local não passou a escutar na rede: %q", c.LocalStoreListen)
	}

	// join no outro servidor.
	t.Setenv("ARKAME_STORAGE_ACCESS_KEY", chave)
	t.Setenv("ARKAME_STORAGE_SECRET_KEY", segredo)
	saida.Reset()
	j := opcoesDoJoin{storageID: o.storageID, bucket: o.bucket, endpoint: endpoint, impressao: impressao, config: filepath.Join(cliente, "agent.env"), semServico: true}
	if err := ligarNaCopiaDaRede(ctx, j, &saida); err != nil {
		t.Fatalf("join: %v\n%s", err, saida.String())
	}
	cfg, err := config.Load(filepath.Join(cliente, "agent-rede-"+o.bucket+".env"), config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID != "7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f" {
		t.Fatalf("o processo da rede não usa a identidade do cliente: %s", cfg.AgentID)
	}
	c, err := storage.NewS3Client(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &cfg.StorageBucket, Key: aws.String("arkame/data/7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f/x.txt"), Body: strings.NewReader("backup do cliente")})
	if err != nil {
		t.Fatalf("o cliente não grava na cópia local da rede: %v", err)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &cfg.StorageBucket, Key: aws.String("arkame/data/7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f/x.txt"), VersionId: put.VersionId}); err == nil {
		t.Fatal("o cliente apagou uma versão na cópia local da rede")
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &cfg.StorageBucket, Key: aws.String("arkame/data/ag-dono/x.txt"), Body: strings.NewReader("estrago")}); err == nil {
		t.Fatal("o cliente gravou na pasta do dono")
	}
	// Um intruso com outro certificado no lugar não passa da impressão.
	if _, err := buscarCertificado(ctx, strings.TrimPrefix(endpoint, "https://"), strings.Repeat("0", 64)); err == nil {
		t.Fatal("a impressão errada passou")
	}

	// revoke: o cliente para de gravar.
	saida.Reset()
	if err := compartilharCopiaLocal(ctx, opcoesDoShare{cfgPath: filepath.Join(dono, "agent.env"), revogar: chave}, &saida); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &cfg.StorageBucket, Key: aws.String("arkame/data/7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f/y.txt"), Body: strings.NewReader("x")}); err == nil {
		t.Fatal("a chave revogada ainda grava")
	}
}
