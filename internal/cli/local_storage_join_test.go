package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

func opcoesDoJoinBoas(endpoint, impressao string) opcoesDoJoin {
	return opcoesDoJoin{storageID: "0b6f0c1e-5f1a-4f6e-9d1e-2a7c3e9b8d41", bucket: "arkame-local-a1b2c3", endpoint: endpoint, impressao: impressao}
}

func TestValidarJoin(t *testing.T) {
	boa := opcoesDoJoinBoas("https://192.168.1.10:9900", strings.Repeat("ab", 32))
	if _, err := boa.validar(); err != nil {
		t.Fatal(err)
	}
	if boa.servico != "arkame-agent-rede-arkame-local-a1b2c3" {
		t.Fatalf("serviço: %s", boa.servico)
	}
	ruins := map[string]opcoesDoJoin{
		"http em texto puro": opcoesDoJoinBoas("http://192.168.1.10:9900", strings.Repeat("ab", 32)),
		"sem porta":          opcoesDoJoinBoas("https://192.168.1.10", strings.Repeat("ab", 32)),
		"com caminho":        opcoesDoJoinBoas("https://192.168.1.10:9900/x", strings.Repeat("ab", 32)),
		"com usuário na URL": opcoesDoJoinBoas("https://a:b@192.168.1.10:9900", strings.Repeat("ab", 32)),
		"impressão curta":    opcoesDoJoinBoas("https://192.168.1.10:9900", "abc"),
		"impressão não hexa": opcoesDoJoinBoas("https://192.168.1.10:9900", strings.Repeat("zz", 32)),
	}
	for nome, o := range ruins {
		if _, err := o.validar(); err == nil {
			t.Errorf("%s: aceito", nome)
		}
	}
	// A impressão com dois-pontos e maiúsculas (como outras ferramentas mostram) é aceita.
	o := opcoesDoJoinBoas("https://192.168.1.10:9900", strings.ToUpper(strings.Repeat("ab:", 31))+"AB")
	if _, err := o.validar(); err != nil {
		t.Fatal(err)
	}
}

// Só o certificado com a impressão do share passa; outro servidor no mesmo
// endereço (um intruso na rede) é recusado.
func TestCertificadoConferidoPelaImpressao(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	h := sha256.Sum256(srv.Certificate().Raw)
	certa := hex.EncodeToString(h[:])
	host := strings.TrimPrefix(srv.URL, "https://")
	pemCert, err := buscarCertificado(context.Background(), host, certa)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pemCert), "BEGIN CERTIFICATE") {
		t.Fatal("não devolveu o PEM")
	}
	if _, err := buscarCertificado(context.Background(), host, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "não confere") {
		t.Fatalf("aceitou a impressão errada: %v", err)
	}
}

// O join grava o processo irmão com a identidade do principal e o certificado
// fixado, e os outros processos do agente deixam o bucket para ele.
func TestJoinGravaOIrmaoENaoDuplica(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	h := sha256.Sum256(srv.Certificate().Raw)
	dir := t.TempDir()
	main := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(main, []byte("PANEL_URL=https://save.arkame.app\nAGENT_ID=ag-1\nSTORAGE_BUCKET=nuvem\nSTORAGE_ID=s-1\nHTTPS_PROXY=http://proxy:3128\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "agent-local.env")
	if err := os.WriteFile(local, []byte("AGENT_ID=ag-1\nSTORAGE_BUCKET=arkame-local-zz\nSIBLING_BUCKETS=nuvem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	antes := credencialDoJoin
	credencialDoJoin = func(_ context.Context, alvo *config.Config) (string, string, error) {
		if alvo.StorageCAFile == "" || !strings.HasPrefix(alvo.StorageEndpoint, "https://") {
			t.Fatalf("testou sem o certificado fixado: %+v", alvo)
		}
		return "arkame-srv-1-abcd", "segredo-da-rede", nil
	}
	t.Cleanup(func() { credencialDoJoin = antes })
	o := opcoesDoJoinBoas(srv.URL, hex.EncodeToString(h[:]))
	o.config, o.semServico = main, true
	var saida strings.Builder
	if err := ligarNaCopiaDaRede(context.Background(), o, &saida); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "agent-rede-arkame-local-a1b2c3.env"), config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	if cfg.AgentID != "ag-1" || cfg.StorageBucket != "arkame-local-a1b2c3" || cfg.StorageEndpoint != "https://"+u.Host || cfg.StorageAccessKey != "arkame-srv-1-abcd" || cfg.StorageCAFile != filepath.Join(dir, "rede", "arkame-local-a1b2c3.pem") {
		t.Fatalf("configuração do irmão: %+v", cfg)
	}
	if cfg.Role != "network_copy" || cfg.AutoUpdate != "follow" {
		t.Fatalf("o processo da rede tem de se apresentar com o papel dele e seguir o principal: role=%q auto=%q", cfg.Role, cfg.AutoUpdate)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent-rede-arkame-local-a1b2c3.env")); !strings.Contains(string(b), "HTTPS_PROXY=http://proxy:3128") {
		t.Fatal("o irmão não seguiu o proxy do principal")
	}
	for _, f := range []string{main, local} {
		c, _ := config.Load(f, config.Overrides{})
		achou := false
		for _, b := range c.SiblingBuckets {
			achou = achou || b == "arkame-local-a1b2c3"
		}
		if !achou {
			t.Fatalf("%s não deixa o bucket da rede para o irmão: %v", f, c.SiblingBuckets)
		}
	}
	// Repetir o join refaz, não recusa.
	if err := ligarNaCopiaDaRede(context.Background(), o, &saida); err != nil {
		t.Fatalf("o join repetido foi recusado: %v", err)
	}
}

func TestInterfacesVirtuaisFicamDeFora(t *testing.T) {
	for _, n := range []string{"docker0", "br-1a2b", "virbr0", "veth12", "podman0", "cni0"} {
		if !interfaceVirtual(n) {
			t.Errorf("%s deveria ficar de fora", n)
		}
	}
	for _, n := range []string{"eth0", "enp3s0", "eno1", "wlan0", "bond0"} {
		if interfaceVirtual(n) {
			t.Errorf("%s é uma interface de rede de verdade", n)
		}
	}
}

func TestServicoDoIrmao(t *testing.T) {
	casos := map[string]string{"/etc/arkame/agent-local.env": "arkame-agent-local", "/etc/arkame/agent-rede-arkame-local-a1.env": "arkame-agent-rede-arkame-local-a1", "/etc/arkame/agent.env": ""}
	for arq, quer := range casos {
		if got := servicoDoIrmao(arq); got != quer {
			t.Errorf("%s: %q, queria %q", arq, got, quer)
		}
	}
}
