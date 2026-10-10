package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arkame-app/agent/internal/atualizacao"
)

// A partida só conta para a prova do programa novo depois de a configuração
// carregar e de o agente ter identidade e token. O `run` com o arquivo errado,
// ou o agente vizinho que já não subia por um problema dele, não gasta as três
// partidas nem desfaz a troca de quem está bem.
func TestRunSoContaAPartidaDepoisDaConfiguracao(t *testing.T) {
	antes, antesSair := registrarArranque, sair
	t.Cleanup(func() { registrarArranque, sair = antes, antesSair })
	var contadas []string
	registrarArranque = func(_, _, chave string) bool {
		contadas = append(contadas, chave)
		return true // manda reverter: o `run` sai antes de subir o daemon
	}
	saiu := -1
	sair = func(codigo int) { saiu = codigo }
	t.Setenv("ARKAME_INSTALL_METHOD", "")

	dir := t.TempDir()
	rodar := func(config string) error {
		cmd := newRunCmd()
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{"--config", config})
		return cmd.Execute()
	}

	// Sem token: o agente não foi aprovado, e não subiria com programa nenhum.
	semToken := filepath.Join(dir, "sem-token.env")
	escrever(t, semToken, "PANEL_URL=http://127.0.0.1:1\nAGENT_ID=a1\nTOKEN_PATH="+filepath.Join(dir, "nao-existe.token")+"\n")
	if err := rodar(semToken); err == nil {
		t.Fatal("o run sem token devia falhar")
	}
	// Com token, sem identidade.
	token := filepath.Join(dir, "agent.token")
	escrever(t, token, "tok\n")
	semId := filepath.Join(dir, "sem-id.env")
	escrever(t, semId, "PANEL_URL=http://127.0.0.1:1\nTOKEN_PATH="+token+"\nAGENT_ID_PATH="+filepath.Join(dir, "nao-existe.id")+"\n")
	if err := rodar(semId); err == nil {
		t.Fatal("o run sem identidade devia falhar")
	}
	if len(contadas) != 0 || saiu != -1 {
		t.Fatalf("a partida que parou antes da configuração contou para a prova: %v (saiu=%d)", contadas, saiu)
	}

	// O agente que sobe de verdade conta, com a própria configuração de chave.
	bom := filepath.Join(dir, "agent.env")
	escrever(t, bom, "PANEL_URL=http://127.0.0.1:1\nAGENT_ID=a1\nTOKEN_PATH="+token+"\n")
	if err := rodar(bom); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(contadas) != 1 || contadas[0] != bom {
		t.Fatalf("partidas contadas = %v, queria só a de %s", contadas, bom)
	}
	if saiu != atualizacao.CodigoDeReinicio {
		t.Fatalf("mandado reverter, o run saiu com %d, queria %d", saiu, atualizacao.CodigoDeReinicio)
	}
}

func escrever(t *testing.T, caminho, conteudo string) {
	t.Helper()
	if err := os.WriteFile(caminho, []byte(conteudo), 0o600); err != nil {
		t.Fatal(err)
	}
}
