package enrollment

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// painelDeEnroll responde o /enroll com resposta e, no wait-token, só entrega
// o token a quem assina com a chave pública recebida no /enroll e o agent_id
// que ele devolveu.
func painelDeEnroll(t *testing.T, resposta string) string {
	t.Helper()
	var (
		mu  sync.Mutex
		pub ed25519.PublicKey
	)
	var devolvido struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal([]byte(resposta), &devolvido)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents/enroll":
			var req api.EnrollRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			pub = ed25519.PublicKey(req.PublicKey)
			mu.Unlock()
			_, _ = io.WriteString(w, resposta)
		case "/w":
			sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Arkame-Signature"))
			material := "arkame-wait-token:" + devolvido.AgentID + ":" + r.Header.Get("X-Arkame-Timestamp")
			mu.Lock()
			ok := len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, []byte(material), sig)
			mu.Unlock()
			if !ok {
				http.Error(w, "assinatura", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"agent_token":"a.b.novo","expires_at":"2027-10-03T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type instalacao struct {
	arquivo, chave, agentID, token string
}

func novaInstalacao(t *testing.T, env string) instalacao {
	t.Helper()
	dir := t.TempDir()
	in := instalacao{
		arquivo: filepath.Join(dir, "agent.env"),
		chave:   filepath.Join(dir, "key.pem"),
		agentID: filepath.Join(dir, "agent.id"),
		token:   filepath.Join(dir, "token.jwt"),
	}
	for p, c := range map[string]string{in.arquivo: env, in.chave: "chave-antiga", in.agentID: "antigo", in.token: "a.b.antigo"} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

func (in instalacao) carregar(t *testing.T, o config.Overrides) *config.Config {
	t.Helper()
	cfg, err := config.Load(in.arquivo, o)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PrivateKeyPath, cfg.AgentIDPath, cfg.TokenPath = in.chave, in.agentID, in.token
	return cfg
}

func ler(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Reinstalação com código novo e a chave do bucket antiga ainda válida: o
// arquivo não era reescrito, o AGENT_ID velho vencia o agent.id novo, e o
// daemon recebia 403 para sempre. Aprovado, o AGENT_ID novo vai ao arquivo.
func TestEnrollmentReescreveAIdentidadeNoArquivo(t *testing.T) {
	in := novaInstalacao(t, "AGENT_ID=antigo\nSTORAGE_ID=st-antigo\nSTORAGE_BUCKET=bucket-antigo\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\nSIBLING_BUCKETS=outro\n")
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w","storage_id":"st-novo","storage_bucket":"bucket-novo"}`)
	t.Setenv("AGENT_ID", "")

	cfg := in.carregar(t, config.Overrides{PanelURL: url, EnrollmentToken: "atk_x"})
	r, err := Run(context.Background(), cfg, Options{Hostname: "h", OS: "linux-amd64"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := WaitForApproval(context.Background(), cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := Concluir(cfg, r, tok.AgentToken); err != nil {
		t.Fatal(err)
	}

	depois := in.carregar(t, config.Overrides{})
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
	if ler(t, in.agentID) != "novo" || ler(t, in.token) != "a.b.novo" || ler(t, in.chave) == "chave-antiga" {
		t.Fatalf("identidade em disco: agent.id=%q token=%q chave trocada=%v",
			ler(t, in.agentID), ler(t, in.token), ler(t, in.chave) != "chave-antiga")
	}
}

// Reinstalação nunca aprovada: o AGENT_ID novo ia ao agent.id e ao env-file
// já no enrollment, com o token antigo no disco — no próximo reinício, 403
// em toda chamada. Agora a identidade antiga fica inteira até a aprovação.
func TestReinstalacaoNaoAprovadaMantemAIdentidadeAntiga(t *testing.T) {
	env := "AGENT_ID=antigo\nSTORAGE_ID=st\nSTORAGE_BUCKET=b\n"
	in := novaInstalacao(t, env)
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w","storage_id":"st-novo","storage_bucket":"b-novo"}`)
	t.Setenv("AGENT_ID", "")

	cfg := in.carregar(t, config.Overrides{PanelURL: url, EnrollmentToken: "atk_x"})
	r, err := Run(context.Background(), cfg, Options{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if r.AgentID != "novo" {
		t.Fatalf("Result.AgentID = %q", r.AgentID)
	}
	if got := ler(t, in.arquivo); got != strings.TrimSpace(env) {
		t.Fatalf("env-file mudou antes da aprovação:\n%s", got)
	}
	if ler(t, in.agentID) != "antigo" || ler(t, in.token) != "a.b.antigo" || ler(t, in.chave) != "chave-antiga" {
		t.Fatalf("identidade antiga mexida antes da aprovação: agent.id=%q token=%q chave=%q",
			ler(t, in.agentID), ler(t, in.token), ler(t, in.chave))
	}
	if depois := in.carregar(t, config.Overrides{}); depois.AgentID != "antigo" || cfg.AgentID != "antigo" {
		t.Fatalf("o daemon subiria com AGENT_ID=%q (memória %q), queria o antigo", depois.AgentID, cfg.AgentID)
	}
}

// Painel que não manda o armazenamento: só o AGENT_ID muda.
func TestEnrollmentSemArmazenamentoMantemOArquivo(t *testing.T) {
	in := novaInstalacao(t, "AGENT_ID=antigo\nSTORAGE_ID=st\nSTORAGE_BUCKET=b\n")
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w"}`)
	t.Setenv("AGENT_ID", "")
	cfg := in.carregar(t, config.Overrides{PanelURL: url, EnrollmentToken: "atk_x"})
	r, err := Run(context.Background(), cfg, Options{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Concluir(cfg, r, "a.b.novo"); err != nil {
		t.Fatal(err)
	}
	depois := in.carregar(t, config.Overrides{})
	if depois.AgentID != "novo" || depois.StorageID != "st" || depois.StorageBucket != "b" {
		t.Fatalf("esperava só o AGENT_ID trocado: %+v", depois)
	}
}

// O código de instalação ficava no env-file depois da aprovação, e o status
// de um agente instalado por env-file mostrava "PENDENTE" para sempre. A
// aprovação o tira do arquivo (e só ele).
func TestAprovacaoTiraOCodigoUsadoDoArquivo(t *testing.T) {
	in := novaInstalacao(t, "ENROLLMENT_TOKEN=atk_x\nAGENT_ID=antigo\nSTORAGE_BUCKET=b\n")
	url := painelDeEnroll(t, `{"agent_id":"novo","status":"pending","wait_url":"/w"}`)
	t.Setenv("AGENT_ID", "")
	t.Setenv("ENROLLMENT_TOKEN", "")
	cfg := in.carregar(t, config.Overrides{PanelURL: url})
	if cfg.EnrollmentToken != "atk_x" {
		t.Fatalf("o código não veio do arquivo: %q", cfg.EnrollmentToken)
	}
	r, err := Run(context.Background(), cfg, Options{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Concluir(cfg, r, "a.b.novo"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ler(t, in.arquivo), "ENROLLMENT_TOKEN") || cfg.EnrollmentToken != "" {
		t.Fatalf("o código usado ficou:\n%s", ler(t, in.arquivo))
	}
	if depois := in.carregar(t, config.Overrides{}); depois.AgentID != "novo" || depois.StorageBucket != "b" {
		t.Fatalf("o resto do arquivo se perdeu: %+v", depois)
	}
}
