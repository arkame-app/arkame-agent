package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

func semearArquivos(t *testing.T, dir string, nomes ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range nomes {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// pastaPadraoIsolada faz a pasta padrão do estado deste sistema cair dentro
// de uma pasta temporária: o uninstall procura nela, e o teste não pode
// enxergar (nem apagar) o estado de um agente de verdade desta máquina.
func pastaPadraoIsolada(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", tmp)
	t.Setenv("ProgramData", tmp)
	return tmp // HostRoot, para o caso de o teste rodar como root
}

func nomesEm(t *testing.T, dir string) []string {
	t.Helper()
	itens, _ := os.ReadDir(dir)
	var nomes []string
	for _, i := range itens {
		nomes = append(nomes, i.Name())
	}
	sort.Strings(nomes)
	return nomes
}

// O uninstall só conhecia o estado do armazenamento da configuração de hoje
// (achado B7). O de um armazenamento anterior do mesmo agente — ele trocou de
// bucket — ficava no disco para sempre, com os nomes dos arquivos do servidor.
// O do vizinho (mesmo agente, outro armazenamento, com configuração própria) e
// o de outro agente continuam ficando.
func TestUninstallRemoveOEstadoDeArmazenamentosAnteriores(t *testing.T) {
	pasta := filepath.Join(t.TempDir(), "arkame", "estado")
	semearArquivos(t, pasta,
		"ag1_st-novo.db", "ag1_st-novo.uso.json",
		"ag1_st-velho.db", "ag1_st-velho.uso.json", "ag1_st-velho.uso.json.tmp",
		"ag1_st-oci.db",
		"ag2_st-novo.db",
		"indice-1.lotes", "ag1_anotacao.txt")
	raiz := pastaPadraoIsolada(t)
	meu := &config.Config{AgentID: "ag1", StorageID: "st-novo", StorageBucket: "b", StateDir: pasta, HostRoot: raiz}
	vizinho := &config.Config{AgentID: "ag1", StorageID: "st-oci", StorageBucket: "c", StateDir: pasta, HostRoot: raiz}

	candidatos, pastas := estadosADesinstalar(meu, "")
	remover, manter := separarEstado(candidatos, []*config.Config{vizinho}, true)
	if len(manter) != 1 || filepath.Base(manter[0]) != "ag1_st-oci.db" {
		t.Fatalf("o estado do vizinho fica: manter=%v", manter)
	}
	removerEstado(remover, pastas, false)
	quero := []string{"ag1_anotacao.txt", "ag1_st-oci.db", "ag2_st-novo.db", "indice-1.lotes"}
	if tem := nomesEm(t, pasta); len(tem) != len(quero) || tem[0] != quero[0] || tem[1] != quero[1] || tem[2] != quero[2] || tem[3] != quero[3] {
		t.Fatalf("ficou %v; queria %v (o estado do armazenamento anterior tinha de sair)", tem, quero)
	}

	// Sem saber de algum outro agente da máquina, nada sai — nem o anterior.
	semearArquivos(t, pasta, "ag1_st-velho.db")
	candidatos, _ = estadosADesinstalar(meu, "")
	if remover, _ := separarEstado(candidatos, nil, false); len(remover) != 0 {
		t.Fatalf("no escuro: remover=%v", remover)
	}
}

// Sem STORAGE_ID na configuração (instalação antiga, ou a linha foi apagada)
// o uninstall não achava estado nenhum.
func TestUninstallSemStorageIDAchaOEstadoDoAgente(t *testing.T) {
	pasta := filepath.Join(t.TempDir(), "estado")
	semearArquivos(t, pasta, "ag1_st-x.db", "ag1_st-x.uso.json", "ag2_st-x.db")
	cfg := &config.Config{AgentID: "ag1", StateDir: pasta, HostRoot: pastaPadraoIsolada(t)}
	candidatos, pastas := estadosADesinstalar(cfg, "")
	remover, _ := separarEstado(candidatos, nil, true)
	removerEstado(remover, pastas, false)
	if tem := nomesEm(t, pasta); len(tem) != 1 || tem[0] != "ag2_st-x.db" {
		t.Fatalf("ficou %v; queria só o do outro agente", tem)
	}
}

// ARKAME_STATE_DIR só na definição do serviço (Environment= posta à mão na
// unit): o uninstall lê a configuração do arquivo e não sabia dessa pasta.
// E a da configuração, quando o estado caiu para lá.
func TestUninstallAchaOEstadoNaPastaDoServicoENaDaConfiguracao(t *testing.T) {
	raiz := t.TempDir()
	isolada := pastaPadraoIsolada(t)
	doServico := filepath.Join(raiz, "dados", "arkame-estado")
	daConfig := filepath.Join(raiz, "etc", "estado")
	semearArquivos(t, doServico, "ag1_st1.db")
	semearArquivos(t, daConfig, "ag1_st1.db")
	// A configuração no arquivo aponta para outra pasta, vazia.
	cfg := &config.Config{AgentID: "ag1", StorageID: "st1", StateDir: filepath.Join(raiz, "padrao"),
		ConfigPath: filepath.Join(raiz, "etc", "agent.env"), HostRoot: isolada}
	candidatos, pastas := estadosADesinstalar(cfg, doServico)
	remover, _ := separarEstado(candidatos, nil, true)
	removerEstado(remover, pastas, true)
	for _, p := range []string{doServico, daConfig} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("a pasta %s devia ter saído, vazia: %v", p, nomesEm(t, p))
		}
	}
	// "off" não é pasta.
	if a, p := estadosADesinstalar(&config.Config{AgentID: "ag1", HostRoot: isolada}, "off"); len(a) != 0 || len(p) != 0 {
		t.Fatalf("off: %v %v", a, p)
	}
}
