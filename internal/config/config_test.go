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

// ARKAME_STATE decide quando o estado local vale: `auto` (o padrão — só com o
// painel que oferece o índice em lotes), `always` (também contra o painel de
// antes) e `off`. E a flag --rehash-all não é a variável ARKAME_REHASH_ALL: a
// flag vale para uma execução, a variável enquanto estiver ligada.
func TestModoDoEstadoERehash(t *testing.T) {
	for _, v := range []string{"ARKAME_STATE", "ARKAME_STATE_DIR", "ARKAME_REHASH_ALL"} {
		t.Setenv(v, "")
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "agent.env")
	carregar := func(linhas string, o Overrides) *Config {
		t.Helper()
		if err := os.WriteFile(env, []byte(linhas), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(env, o)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	pasta := "ARKAME_STATE_DIR=" + filepath.Join(dir, "estado") + "\n"

	if cfg := carregar(pasta, Overrides{}); cfg.StateDir == "" || cfg.StateAlways {
		t.Fatalf("padrão: queria a pasta e StateAlways=false; %q %v", cfg.StateDir, cfg.StateAlways)
	}
	if cfg := carregar(pasta+"ARKAME_STATE=always\n", Overrides{}); cfg.StateDir == "" || !cfg.StateAlways {
		t.Fatalf("always: %q %v", cfg.StateDir, cfg.StateAlways)
	}
	if cfg := carregar(pasta+"ARKAME_STATE=off\n", Overrides{}); cfg.StateDir != "" || cfg.StateAlways {
		t.Fatalf("off: %q %v", cfg.StateDir, cfg.StateAlways)
	}
	if cfg := carregar("ARKAME_STATE_DIR=off\nARKAME_STATE=always\n", Overrides{}); cfg.StateDir != "" || cfg.StateAlways {
		t.Fatalf("pasta off vence o always: %q %v", cfg.StateDir, cfg.StateAlways)
	}
	if cfg := carregar(pasta+"ARKAME_STATE=qualquer\n", Overrides{}); cfg.StateAlways {
		t.Fatal("valor desconhecido vale auto")
	}

	if cfg := carregar(pasta, Overrides{RehashAll: true}); !cfg.RehashAllOnce || cfg.RehashAll {
		t.Fatalf("flag: queria RehashAllOnce só; once=%v sempre=%v", cfg.RehashAllOnce, cfg.RehashAll)
	}
	if cfg := carregar(pasta+"ARKAME_REHASH_ALL=1\n", Overrides{}); cfg.RehashAllOnce || !cfg.RehashAll {
		t.Fatalf("variável: queria RehashAll só; once=%v sempre=%v", cfg.RehashAllOnce, cfg.RehashAll)
	}
}
