package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Sem AGENT_ID no env-file (chave já estava lá, ou --check-storage=false), o
// enrollment só grava o agent.id. A config tem de achá-lo — senão o daemon
// chama /api/agents//plans para sempre.
func TestAgentIDCaiNoArquivoDoEnrollment(t *testing.T) {
	t.Setenv("AGENT_ID", "")
	dir := t.TempDir()
	id := filepath.Join(dir, "agent.id")
	if err := os.WriteFile(id, []byte("01HAGENTE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(env, []byte("AGENT_ID_PATH="+id+"\nSTORAGE_BUCKET=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID != "01HAGENTE" {
		t.Fatalf("AgentID = %q, queria o do agent.id", cfg.AgentID)
	}

	// O env-file, quando diz, continua valendo.
	if err := os.WriteFile(env, []byte("AGENT_ID=01HDOARQUIVO\nAGENT_ID_PATH="+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Load(env, Overrides{}); cfg.AgentID != "01HDOARQUIVO" {
		t.Fatalf("AgentID = %q, queria o do env-file", cfg.AgentID)
	}
}

// Um token.jwt que voltou com 0 bytes de um corte de energia não é token: o
// os.Stat dizia que sim, e o agente subia sem conseguir se autenticar.
func TestTokenExistsArquivoVazioNaoConta(t *testing.T) {
	dir := t.TempDir()
	casos := []struct {
		nome     string
		conteudo *string
		quer     bool
	}{
		{"inexistente", nil, false},
		{"vazio", ptr(""), false},
		{"só espaços", ptr(" \n"), false},
		{"com token", ptr("eyJhbGciOi.x.y\n"), true},
	}
	for _, c := range casos {
		p := filepath.Join(dir, c.nome+".jwt")
		if c.conteudo != nil {
			if err := os.WriteFile(p, []byte(*c.conteudo), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg := &Config{TokenPath: p}
		if got := cfg.TokenExists(); got != c.quer {
			t.Errorf("%s: TokenExists=%v, queria %v", c.nome, got, c.quer)
		}
	}
}

func ptr(s string) *string { return &s }
