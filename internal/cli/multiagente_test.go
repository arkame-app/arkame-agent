package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// Dois agentes na máquina com a identidade nos caminhos padrão: desinstalar
// um apagava o token, a chave e o agent.id do outro.
func TestUninstallNaoApagaIdentidadeDeOutroAgente(t *testing.T) {
	meu := "/etc/arkame/agent-aws.env"
	cand := []string{meu, config.DefaultTokenPath, config.DefaultPrivateKeyPath, config.DefaultAgentIDPath}
	vizinho := &config.Config{ConfigPath: "/etc/arkame/agent.env", TokenPath: config.DefaultTokenPath,
		PrivateKeyPath: config.DefaultPrivateKeyPath, AgentIDPath: config.DefaultAgentIDPath}

	remover, manter := separarArquivos(meu, cand, []*config.Config{vizinho}, true)
	if !slices.Equal(remover, []string{meu}) {
		t.Fatalf("removeria %v; só o arquivo próprio deveria sair", remover)
	}
	if len(manter) != 3 {
		t.Fatalf("mantém %v", manter)
	}

	// Vizinho com identidade própria: a minha sai toda.
	vizinho2 := &config.Config{ConfigPath: "/etc/arkame/agent.env", TokenPath: "/etc/arkame/agent.token.jwt",
		PrivateKeyPath: "/etc/arkame/agent.key.pem", AgentIDPath: "/etc/arkame/agent.agent.id"}
	if remover, _ := separarArquivos(meu, cand, []*config.Config{vizinho2}, true); len(remover) != 4 {
		t.Fatalf("sem caminho em comum, deveria remover tudo: %v", remover)
	}

	// Sem nenhum outro agente: tudo sai.
	if remover, manter := separarArquivos(meu, cand, nil, true); len(remover) != 4 || len(manter) != 0 {
		t.Fatalf("único agente: remover=%v manter=%v", remover, manter)
	}

	// Outro agente cuja configuração não deu para ler: a identidade fica.
	remover, manter = separarArquivos(meu, cand, nil, false)
	if !slices.Equal(remover, []string{meu}) || len(manter) != 3 {
		t.Fatalf("no escuro: remover=%v manter=%v", remover, manter)
	}
}

// O segundo agente (--config próprio) ganha identidade ao lado do arquivo
// dele, em vez de gravar por cima da do agente padrão.
func TestInstallComConfigPropriaSeparaAIdentidade(t *testing.T) {
	for _, k := range []string{"TOKEN_PATH", "PRIVATE_KEY_PATH", "AGENT_ID_PATH"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	arquivo := filepath.Join(dir, "agent-oci.env")
	if err := os.WriteFile(arquivo, []byte("STORAGE_BUCKET=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(arquivo, config.Overrides{})
	mudou, err := identidadePropria(arquivo, cfg)
	if err != nil || !mudou {
		t.Fatalf("mudou=%v err=%v", mudou, err)
	}
	cfg, _ = config.Load(arquivo, config.Overrides{})
	base := filepath.Join(dir, "agent-oci")
	if cfg.TokenPath != base+".token.jwt" || cfg.PrivateKeyPath != base+".key.pem" || cfg.AgentIDPath != base+".agent.id" {
		t.Fatalf("identidade: %s %s %s", cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath)
	}
	if cfg.StorageBucket != "b" {
		t.Fatal("o resto do arquivo se perdeu")
	}
	// De novo: nada muda.
	if mudou, _ := identidadePropria(arquivo, cfg); mudou {
		t.Fatal("reescreveu caminhos já definidos")
	}
	// A configuração padrão segue com os caminhos padrão.
	padrao, _ := filepath.Abs(config.DefaultPath)
	if mudou, _ := identidadePropria(padrao, &config.Config{TokenPath: config.DefaultTokenPath}); mudou {
		t.Fatal("mexeu na configuração padrão")
	}
}
