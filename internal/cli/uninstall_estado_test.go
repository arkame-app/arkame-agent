package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/daemon"
)

// O uninstall apaga o estado local do backup — ele guarda os nomes dos
// arquivos do servidor —, mas só o deste agente + armazenamento: o de outro
// agente da máquina, na mesma pasta, fica; e sem saber de algum outro agente,
// fica tudo.
func TestUninstallRemoveOEstadoLocalSoDesteAgente(t *testing.T) {
	raiz := t.TempDir()
	pasta := filepath.Join(raiz, "arkame", "estado")
	if err := os.MkdirAll(pasta, 0o700); err != nil {
		t.Fatal(err)
	}
	meu := &config.Config{AgentID: "ag1", StorageID: "st-aws", StorageBucket: "b", StateDir: pasta}
	vizinho := &config.Config{AgentID: "ag1", StorageID: "st-oci", StorageBucket: "c", StateDir: pasta}
	semear := func(cfg *config.Config) []string {
		t.Helper()
		arquivos, _ := daemon.ArquivosDoEstado(cfg)
		for _, a := range arquivos[:2] { // o estado e a medição de ocupação
			if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return arquivos[:2]
	}
	meus, deles := semear(meu), semear(vizinho)
	existe := func(p string) bool { _, err := os.Stat(p); return err == nil }

	// Sem saber do vizinho: nada sai.
	candidatos, pastas := daemon.ArquivosDoEstado(meu)
	if remover, manter := separarEstado(candidatos, nil, false); len(remover) != 0 || len(manter) != 2 {
		t.Fatalf("no escuro: remover=%v manter=%v", remover, manter)
	}
	// Vizinho com o MESMO agente e armazenamento (duas configurações da mesma
	// instalação): o estado é dele também, e fica.
	gemeo := &config.Config{AgentID: "ag1", StorageID: "st-aws", StorageBucket: "b", StateDir: pasta}
	if remover, manter := separarEstado(candidatos, []*config.Config{gemeo}, true); len(remover) != 0 || len(manter) != 2 {
		t.Fatalf("gêmeo: remover=%v manter=%v", remover, manter)
	}

	// Vizinho de outro armazenamento: só os meus saem; a pasta fica (não está vazia).
	remover, manter := separarEstado(candidatos, []*config.Config{vizinho}, true)
	if len(remover) != 2 || len(manter) != 0 {
		t.Fatalf("remover=%v manter=%v", remover, manter)
	}
	if saiu, ficou := removerEstado(remover, pastas, false); len(saiu) != 2 || len(ficou) != 0 {
		t.Fatalf("saiu=%v ficou=%v", saiu, ficou)
	}
	for _, p := range meus {
		if existe(p) {
			t.Fatalf("o estado deste agente ficou: %s", p)
		}
	}
	for _, p := range deles {
		if !existe(p) {
			t.Fatalf("o estado do vizinho foi apagado: %s", p)
		}
	}
	if !existe(pasta) {
		t.Fatal("a pasta do estado saiu com o estado do vizinho dentro")
	}

	// O último agente: sai o estado, um temporário de índice órfão, a pasta e
	// a `arkame` acima — mas não a pasta de quem a hospeda.
	orfao := filepath.Join(pasta, "indice-123.lotes")
	_ = os.WriteFile(orfao, []byte("x"), 0o600)
	candidatos, pastas = daemon.ArquivosDoEstado(vizinho)
	remover, _ = separarEstado(candidatos, nil, true)
	removerEstado(remover, pastas, true)
	if existe(pasta) || existe(filepath.Dir(pasta)) {
		t.Fatal("a pasta do estado (e a arkame acima) devia ter saído vazia")
	}
	if !existe(raiz) {
		t.Fatal("apagou a pasta acima da do agente")
	}
}
