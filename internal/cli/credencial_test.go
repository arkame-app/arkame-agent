package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/terminal"
)

// s3Falso aceita GetBucketVersioning só para os pares bucket → chaves dados;
// o resto leva 403 AccessDenied (a chave não serve naquele bucket).
func s3Falso(t *testing.T, aceitas map[string][]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket := strings.Trim(r.URL.Path, "/")
		auth := r.Header.Get("Authorization")
		for _, ak := range aceitas[bucket] {
			if strings.Contains(auth, "Credential="+ak+"/") {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
				return
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// painelDeInstalacao responde o install-config com o armazenamento dado.
func painelDeInstalacao(t *testing.T, armazenamento string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents/install-config" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"agent_id":"ag-novo","display_name":"Srv","storage":`+armazenamento+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func semTerminal(t *testing.T) {
	t.Helper()
	antes := abrirTerminal
	abrirTerminal = func() (*terminal.Terminal, error) { return nil, terminal.ErrSemTerminal }
	t.Cleanup(func() { abrirTerminal = antes })
}

func isolarAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY", "STORAGE_ENDPOINT", "STORAGE_REGION", "STORAGE_BUCKET", "STORAGE_ID", "AGENT_ID", "PANEL_URL", "ENROLLMENT_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func arquivoDeConfig(t *testing.T, conteudo string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(p, []byte(conteudo), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func carregarCfg(t *testing.T, arquivo, painel string) *config.Config {
	t.Helper()
	cfg, err := config.Load(arquivo, config.Overrides{PanelURL: painel, EnrollmentToken: "atk_x"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Reinstalação com chave no arquivo e código de outro armazenamento: o teste
// olhava só o bucket velho do arquivo, e a aprovação gravava o bucket novo com
// a chave, a região e o endereço do velho. Sem terminal, agora para com a
// causa, sem mexer no arquivo.
func TestChaveDoArquivoTestadaNoArmazenamentoDoCodigo(t *testing.T) {
	isolarAWS(t)
	semTerminal(t)
	s3 := s3Falso(t, map[string][]string{"velho": {"ak-velha"}, "novo": {"ak-nova"}})
	painel := painelDeInstalacao(t, fmt.Sprintf(`{"id":"st-novo","display_name":"Novo","bucket":"novo","region":"sa-east-1","endpoint":%q}`, s3))
	conteudo := fmt.Sprintf("AGENT_ID=ag-velho\nSTORAGE_ID=st-velho\nSTORAGE_BUCKET=velho\nSTORAGE_REGION=us-west-2\nSTORAGE_ENDPOINT=%s\nSTORAGE_ACCESS_KEY=ak-velha\nSTORAGE_SECRET_KEY=sk\n", s3)
	arquivo := arquivoDeConfig(t, conteudo)

	linhas, err := garantirCredencial(context.Background(), carregarCfg(t, arquivo, painel), arquivo)
	if err == nil {
		t.Fatalf("a chave do bucket velho passou como se servisse ao novo (linhas=%v)", linhas)
	}
	if !strings.Contains(err.Error(), "novo") {
		t.Fatalf("o erro não diz qual bucket recusou: %v", err)
	}
	if b, _ := os.ReadFile(arquivo); string(b) != conteudo {
		t.Fatalf("o arquivo mudou sem chave que sirva:\n%s", b)
	}
}

// A chave do arquivo serve no armazenamento novo: devolve o armazenamento
// novo inteiro (região e endereço juntos), mantém a chave e não toca no
// AGENT_ID. O arquivo não muda: as linhas só vão ao disco com a aprovação.
func TestArmazenamentoNovoGravadoInteiro(t *testing.T) {
	isolarAWS(t)
	semTerminal(t)
	s3 := s3Falso(t, map[string][]string{"velho": {"ak"}, "novo": {"ak"}})
	painel := painelDeInstalacao(t, fmt.Sprintf(`{"id":"st-novo","display_name":"Novo","bucket":"novo","region":"sa-east-1","endpoint":%q}`, s3+"/"))
	conteudo := fmt.Sprintf("AGENT_ID=ag-velho\nSTORAGE_ID=st-velho\nSTORAGE_BUCKET=velho\nSTORAGE_REGION=us-west-2\nSTORAGE_ENDPOINT=%s\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\n", s3)
	arquivo := arquivoDeConfig(t, conteudo)

	linhas, err := garantirCredencial(context.Background(), carregarCfg(t, arquivo, painel), arquivo)
	if err != nil || len(linhas) == 0 {
		t.Fatalf("linhas=%v err=%v", linhas, err)
	}
	if b, _ := os.ReadFile(arquivo); string(b) != conteudo {
		t.Fatalf("o arquivo mudou antes da aprovação:\n%s", b)
	}
	depois, _ := config.Load(arquivo, config.Overrides{Pendentes: linhas})
	if depois.StorageID != "st-novo" || depois.StorageBucket != "novo" || depois.StorageRegion != "sa-east-1" || depois.StorageEndpoint != s3+"/" {
		t.Fatalf("armazenamento: id=%q bucket=%q região=%q endereço=%q", depois.StorageID, depois.StorageBucket, depois.StorageRegion, depois.StorageEndpoint)
	}
	if depois.StorageAccessKey != "ak" || depois.StorageSecretKey != "sk" || depois.AgentID != "ag-velho" {
		t.Fatalf("chave ou AGENT_ID mexidos: %+v", depois)
	}
}

// Mesmo armazenamento no código e no arquivo: testa e não reescreve nada.
func TestMesmoArmazenamentoNaoReescreve(t *testing.T) {
	isolarAWS(t)
	semTerminal(t)
	s3 := s3Falso(t, map[string][]string{"b": {"ak"}})
	painel := painelDeInstalacao(t, fmt.Sprintf(`{"id":"st","display_name":"B","bucket":"b","region":null,"endpoint":%q}`, s3))
	conteudo := fmt.Sprintf("STORAGE_ID=st\nSTORAGE_BUCKET=b\nSTORAGE_ENDPOINT=%s\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\n", s3)
	arquivo := arquivoDeConfig(t, conteudo)

	linhas, err := garantirCredencial(context.Background(), carregarCfg(t, arquivo, painel), arquivo)
	if err != nil || linhas != nil {
		t.Fatalf("linhas=%v err=%v", linhas, err)
	}
	if b, _ := os.ReadFile(arquivo); string(b) != conteudo {
		t.Fatalf("o arquivo mudou:\n%s", b)
	}
}
