package enrollment

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

func painelDeEnroll(t *testing.T, resposta string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/api/agents/enroll" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, resposta)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Reinstalação com código novo e a chave do bucket antiga ainda válida: o
// arquivo não era reescrito, o AGENT_ID velho vencia o agent.id novo, e o
// daemon recebia 403 para sempre.
func TestEnrollmentReescreveAIdentidadeNoArquivo(t *testing.T) {
	dir := t.TempDir()
	arquivo := filepath.Join(dir, "agent.env")
	antigo := "AGENT_ID=antigo\nSTORAGE_ID=st-antigo\nSTORAGE_BUCKET=bucket-antigo\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\nSIBLING_BUCKETS=outro\n"
	if err := os.WriteFile(arquivo, []byte(antigo), 0o600); err != nil {
		t.Fatal(err)
	}
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w","storage_id":"st-novo","storage_bucket":"bucket-novo"}`)
	t.Setenv("AGENT_ID", "")

	cfg, err := config.Load(arquivo, config.Overrides{PanelURL: url, EnrollmentToken: "atk_x"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.PrivateKeyPath = filepath.Join(dir, "key.pem")
	cfg.AgentIDPath = filepath.Join(dir, "agent.id")
	if _, err := Run(context.Background(), cfg, Options{Hostname: "h", OS: "linux-amd64"}); err != nil {
		t.Fatal(err)
	}

	depois, err := config.Load(arquivo, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if depois.AgentID != "novo" {
		t.Fatalf("o daemon subiria com AGENT_ID=%q, queria o novo", depois.AgentID)
	}
	if depois.StorageID != "st-novo" || depois.StorageBucket != "bucket-novo" {
		t.Fatalf("armazenamento do enrollment não gravado: %q %q", depois.StorageID, depois.StorageBucket)
	}
	if depois.StorageAccessKey != "ak" || depois.StorageSecretKey != "sk" || len(depois.SiblingBuckets) != 1 {
		t.Fatalf("o resto do arquivo se perdeu: %+v", depois)
	}
	if cfg.AgentID != "novo" {
		t.Fatalf("a config em memória ficou com %q", cfg.AgentID)
	}
}

// Painel que não manda o armazenamento: só o AGENT_ID muda.
func TestEnrollmentSemArmazenamentoMantemOArquivo(t *testing.T) {
	dir := t.TempDir()
	arquivo := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(arquivo, []byte("AGENT_ID=antigo\nSTORAGE_ID=st\nSTORAGE_BUCKET=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w"}`)
	t.Setenv("AGENT_ID", "")
	cfg, _ := config.Load(arquivo, config.Overrides{PanelURL: url, EnrollmentToken: "atk_x"})
	cfg.PrivateKeyPath = filepath.Join(dir, "key.pem")
	cfg.AgentIDPath = filepath.Join(dir, "agent.id")
	if _, err := Run(context.Background(), cfg, Options{Hostname: "h"}); err != nil {
		t.Fatal(err)
	}
	depois, _ := config.Load(arquivo, config.Overrides{})
	if depois.AgentID != "novo" || depois.StorageID != "st" || depois.StorageBucket != "b" {
		t.Fatalf("esperava só o AGENT_ID trocado: %+v", depois)
	}
}
