package cli

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
	"github.com/arkame-app/agent/internal/service"
	"github.com/spf13/cobra"
)

func jwtQueVence(t *testing.T, venc time.Time) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":"ag","exp":%d}`, venc.Unix())))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".assinatura"
}

func statusDe(t *testing.T, cfg *config.Config, agora time.Time) string {
	t.Helper()
	var b bytes.Buffer
	escreverStatus(&b, cfg, agora)
	return b.String()
}

// Agente aprovado e instalado por env-file com ENROLLMENT_TOKEN: o status
// dizia "PENDENTE" para sempre, e a fingerprint "(não configurado)" porque
// nada gravava o AGENT_FINGERPRINT.
func TestStatusDeAgenteAprovado(t *testing.T) {
	dir := t.TempDir()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	chave := filepath.Join(dir, "key.pem")
	if err := kp.SaveToDisk(chave); err != nil {
		t.Fatal(err)
	}
	agora := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	token := filepath.Join(dir, "token.jwt")
	if err := os.WriteFile(token, []byte(jwtQueVence(t, agora.AddDate(1, 0, 0))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AgentID: "ag", EnrollmentToken: "atk_usado", PrivateKeyPath: chave, TokenPath: token}

	out := statusDe(t, cfg, agora)
	if !strings.Contains(out, crypto.Fingerprint(kp.Public)) {
		t.Errorf("fingerprint da chave ausente:\n%s", out)
	}
	data := func(t time.Time) string { return time.Unix(t.Unix(), 0).Format("2006-01-02") } // no fuso local, como o status mostra
	if strings.Contains(out, "PENDENTE") || !strings.Contains(out, "APROVADO (token válido até "+data(agora.AddDate(1, 0, 0))+")") {
		t.Errorf("agente aprovado com situação errada:\n%s", out)
	}

	// Token vencido: diz, em vez de "APROVADO".
	if err := os.WriteFile(token, []byte(jwtQueVence(t, agora.AddDate(0, 0, -1))), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := statusDe(t, cfg, agora); !strings.Contains(out, "TOKEN VENCIDO em "+data(agora.AddDate(0, 0, -1))) {
		t.Errorf("token vencido não aparece:\n%s", out)
	}

	// Sem token, com o código: install não concluído.
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if out := statusDe(t, cfg, agora); !strings.Contains(out, "PENDENTE") {
		t.Errorf("sem token, com código, deveria ser PENDENTE:\n%s", out)
	}
	cfg.EnrollmentToken = ""
	if out := statusDe(t, cfg, agora); !strings.Contains(out, "NÃO INICIADO") {
		t.Errorf("sem token nem código, deveria ser NÃO INICIADO:\n%s", out)
	}
	if out := statusDe(t, &config.Config{PrivateKeyPath: filepath.Join(dir, "nao-tem.pem")}, agora); !strings.Contains(out, "(sem chave em") {
		t.Errorf("sem chave, deveria dizer:\n%s", out)
	}
}

// A ajuda prometia o último heartbeat, que o status não mostra (só o painel
// sabe).
func TestAjudaDoStatusNaoPrometeHeartbeat(t *testing.T) {
	c := newStatusCmd()
	if strings.Contains(c.Short, "heartbeat") {
		t.Fatalf("a ajuda do status promete o heartbeat: %q", c.Short)
	}
}

// Agente sem root (~/.config/arkame/agent.env) ou segundo agente: o status
// lia só /etc/arkame/agent.env e dizia "NÃO INICIADO — rode install" de um
// agente rodando. Sem --config, o arquivo é o do serviço, como no
// set-storage-keys e no uninstall; o heartbeat segue a mesma regra.
func TestStatusEHeartbeatLeemOArquivoDoServico(t *testing.T) {
	for _, k := range []string{"AGENT_ID", "TOKEN_PATH", "ENROLLMENT_TOKEN", "PRIVATE_KEY_PATH", "PANEL_URL"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	escrever := func(nome, agentID string) string {
		p := filepath.Join(dir, nome)
		conteudo := "AGENT_ID=" + agentID + "\nTOKEN_PATH=" + filepath.Join(dir, nome+".token") +
			"\nPRIVATE_KEY_PATH=" + filepath.Join(dir, nome+".pem") + "\nENROLLMENT_TOKEN=atk_x\nPANEL_URL=https://painel.invalid\n"
		if err := os.WriteFile(p, []byte(conteudo), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	doUsuario := escrever("usuario.env", "ag-usuario")
	doOutro := escrever("oci.env", "ag-oci")

	antes := configDoServico
	configDoServico = func(nome string, escopo service.Scope) (string, bool) {
		switch {
		case nome == service.DefaultName && escopo == "":
			return doUsuario, true // escopo atual: o registro do usuário
		case nome == "arkame-agent-oci":
			return doOutro, true
		}
		return "", false
	}
	t.Cleanup(func() { configDoServico = antes })

	rodar := func(c *cobra.Command, args ...string) (string, error) {
		var b bytes.Buffer
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		err := c.Execute()
		return b.String(), err
	}

	out, err := rodar(newStatusCmd())
	if err != nil || !strings.Contains(out, "ag-usuario") || !strings.Contains(out, doUsuario) || !strings.Contains(out, "PENDENTE") {
		t.Fatalf("status sem flags deveria ler %s; err=%v\n%s", doUsuario, err, out)
	}
	out, err = rodar(newStatusCmd(), "--service-name", "arkame-agent-oci")
	if err != nil || !strings.Contains(out, "ag-oci") {
		t.Fatalf("status --service-name deveria ler %s; err=%v\n%s", doOutro, err, out)
	}
	if _, err := rodar(newStatusCmd(), "--service-name", "arkame-agent-sumido"); err == nil || !strings.Contains(err.Error(), "arkame-agent-sumido") {
		t.Fatalf("serviço sem registro deveria dar erro, veio %v", err)
	}
	// --config explícito vence o registro.
	if out, err := rodar(newStatusCmd(), "--config", doOutro); err != nil || !strings.Contains(out, "ag-oci") {
		t.Fatalf("--config explícito: err=%v\n%s", err, out)
	}

	// heartbeat: chega até o token do arquivo do serviço (que não existe
	// aqui) em vez de dizer que falta o AGENT_ID do arquivo padrão.
	if _, err := rodar(newHeartbeatCmd(), "--service-name", "arkame-agent-oci"); err == nil || !strings.Contains(err.Error(), "bearer token ausente") {
		t.Fatalf("heartbeat deveria ler %s, veio %v", doOutro, err)
	}
}
