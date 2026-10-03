package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
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
	linhas, err := identidadePropria(arquivo, cfg)
	if err != nil || len(linhas) == 0 {
		t.Fatalf("linhas=%v err=%v", linhas, err)
	}
	if b, _ := os.ReadFile(arquivo); string(b) != "STORAGE_BUCKET=b\n" {
		t.Fatalf("o arquivo mudou antes da aprovação:\n%s", b)
	}
	cfg, _ = config.Load(arquivo, config.Overrides{Pendentes: linhas})
	base := filepath.Join(dir, "agent-oci")
	if cfg.TokenPath != base+".token.jwt" || cfg.PrivateKeyPath != base+".key.pem" || cfg.AgentIDPath != base+".agent.id" {
		t.Fatalf("identidade: %s %s %s", cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath)
	}
	if cfg.StorageBucket != "b" {
		t.Fatal("o resto do arquivo se perdeu")
	}
	// De novo: nada muda.
	if linhas, _ := identidadePropria(arquivo, cfg); linhas != nil {
		t.Fatal("reescreveu caminhos já definidos")
	}
	// A configuração padrão segue com os caminhos padrão.
	padrao, _ := filepath.Abs(config.DefaultPath)
	if linhas, _ := identidadePropria(padrao, &config.Config{TokenPath: config.DefaultTokenPath}); linhas != nil {
		t.Fatal("mexeu na configuração padrão")
	}
}

// Com --service-name do segundo agente e sem --config, set-storage-keys e
// uninstall usam o arquivo do serviço dele, e não o padrão (o do agente
// principal): antes, a chave era testada e gravada no agente errado, e o
// uninstall apagava a configuração do vizinho.
func TestServiceNameUsaOArquivoDoProprioServico(t *testing.T) {
	t.Setenv("STORAGE_BUCKET", "")
	dir := t.TempDir()
	doOutro := filepath.Join(dir, "agent-oci.env")
	if err := os.WriteFile(doOutro, []byte("PANEL_URL=https://x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pedidos := []string{}
	antes := configDoServico
	configDoServico = func(nome string, _ service.Scope) (string, bool) {
		pedidos = append(pedidos, nome)
		if nome == "arkame-agent-oci" {
			return doOutro, true
		}
		return "", false
	}
	t.Cleanup(func() { configDoServico = antes })

	// set-storage-keys: o arquivo sem STORAGE_BUCKET é o do serviço pedido.
	cmd := newSetStorageKeysCmd()
	cmd.SetArgs([]string{"--service-name", "arkame-agent-oci"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), doOutro) {
		t.Fatalf("set-storage-keys deveria usar %s, veio %v", doOutro, err)
	}

	// uninstall: o serviço sem registro legível não cai no arquivo padrão.
	cmd = newUninstallCmd()
	cmd.SetArgs([]string{"--service-name", "arkame-agent-sumido", "--yes"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "arkame-agent-sumido") || !strings.Contains(err.Error(), "nada foi removido") {
		t.Fatalf("uninstall deveria parar sem achar o arquivo do serviço, veio %v", err)
	}

	// --config explícito vence, e o serviço padrão sem registro segue no
	// arquivo padrão do escopo (/etc/arkame como root, o do usuário sem).
	c := newUninstallCmd()
	_ = c.ParseFlags([]string{"--config", "/x/y.env", "--service-name", "arkame-agent-oci"})
	if got, _ := configDoAgente(c, "/x/y.env", "arkame-agent-oci", ""); got != "/x/y.env" {
		t.Fatalf("--config explícito virou %s", got)
	}
	c = newUninstallCmd()
	if got, _ := configDoAgente(c, config.DefaultPath, "arkame-agent", ""); got != configPadrao("") {
		t.Fatalf("serviço padrão virou %s", got)
	}
	if !slices.Equal(pedidos, []string{"arkame-agent-oci", "arkame-agent-sumido", "arkame-agent"}) {
		t.Fatalf("consultas ao registro: %v", pedidos)
	}
}

// Agente rootless com o nome padrão: o comando do painel (`set-storage-keys
// --restart --service-scope user`, sem --config) lia /etc/arkame/agent.env,
// que não existe, em vez de ~/.config/arkame/agent.env da unit do usuário.
func TestNomePadraoRootlessUsaOArquivoDoServicoDoUsuario(t *testing.T) {
	t.Setenv("STORAGE_BUCKET", "")
	dir := t.TempDir()
	doUsuario := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(doUsuario, []byte("PANEL_URL=https://x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type pedido struct {
		nome   string
		escopo service.Scope
	}
	var pedidos []pedido
	antes := configDoServico
	configDoServico = func(nome string, escopo service.Scope) (string, bool) {
		pedidos = append(pedidos, pedido{nome, escopo})
		if nome == service.DefaultName && escopo == service.ScopeUser {
			return doUsuario, true
		}
		return "", false
	}
	t.Cleanup(func() { configDoServico = antes })

	cmd := newSetStorageKeysCmd()
	cmd.SetArgs([]string{"--restart", "--service-scope", "user"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), doUsuario) {
		t.Fatalf("set-storage-keys deveria usar %s, veio %v", doUsuario, err)
	}
	if len(pedidos) != 1 || pedidos[0] != (pedido{service.DefaultName, service.ScopeUser}) {
		t.Fatalf("consultas ao registro: %v", pedidos)
	}

	// uninstall também: a configuração do usuário é a que sai.
	c := newUninstallCmd()
	_ = c.ParseFlags([]string{"--service-scope", "user"})
	if got, err := configDoAgente(c, config.DefaultPath, service.DefaultName, "user"); err != nil || got != doUsuario {
		t.Fatalf("uninstall: %q, %v", got, err)
	}
	// Sem registro no escopo pedido, o nome padrão fica no arquivo padrão.
	if got, err := configDoAgente(c, config.DefaultPath, service.DefaultName, "system"); err != nil || got != config.DefaultPath {
		t.Fatalf("sem registro: %q, %v", got, err)
	}
}
