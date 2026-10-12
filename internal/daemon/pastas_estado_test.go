package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arkame-app/agent/internal/estado"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// versaoNoEstado devolve a versão que o arquivo de estado em caminho afirma
// para a chave (relativa ao prefixo do plano).
func (m *mesaDoDaemon) versaoNoEstado(caminho, chave string) (string, bool) {
	m.t.Helper()
	b, err := estado.AbrirExistente(caminho)
	if err != nil {
		m.t.Fatal(err)
	}
	defer b.Fechar()
	prefixo := syncengine.PrefixoDasChaves("data/" + m.cfg.AgentID + "/")
	p, err := b.Plano(m.plano.ID, prefixo)
	if err != nil {
		m.t.Fatal(err)
	}
	ex, err := p.Comecar("leitura")
	if err != nil {
		m.t.Fatal(err)
	}
	ent, achou, err := ex.Buscar(chave[len(prefixo):])
	if err != nil {
		m.t.Fatal(err)
	}
	return ent.Versao, achou
}

// Estado em uso por outro processo (agravante do achado M1): o agente abria
// um SEGUNDO estado na pasta da configuração, que passava a envelhecer ao lado
// do primeiro. A segunda pasta é para quando a primeira não aceita escrita;
// com o estado em uso, o backup roda sem estado — lê e confere tudo.
func TestEstadoEmUsoNaoCriaUmSegundoEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.ConfigPath = filepath.Join(t.TempDir(), "agent.env")
	alternativa := filepath.Join(filepath.Dir(m.cfg.ConfigPath), "estado")
	m.semear(25)
	m.rodarOK()
	if _, err := os.Stat(alternativa); err == nil {
		t.Fatal("com a pasta principal em ordem a alternativa não é criada")
	}

	ocupado, err := estado.Abrir(m.cfg.StateDir, identidadeDoEstado(m.cfg, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m.s3.Zerar()
	s := m.rodarOK()
	_ = ocupado.Fechar()
	if s.Status != "complete" || s.Modo != "full" || m.s3.N("HeadObject") != 25 || m.s3.N("PutObject") != 0 {
		t.Fatalf("sem estado o backup confere tudo e não reenvia: %+v s3=%v", s, m.s3.Requisicoes())
	}
	if _, err := os.Stat(arquivoDoEstado(m.cfg, alternativa)); err == nil {
		t.Fatal("um segundo arquivo de estado foi criado na pasta da configuração")
	}
	m.conferir()
}

// Pasta principal sem escrita: aí sim o estado vai para a da configuração.
func TestPastaDoEstadoSemEscritaCaiParaADaConfiguracao(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.ConfigPath = filepath.Join(t.TempDir(), "agent.env")
	alternativa := filepath.Join(filepath.Dir(m.cfg.ConfigPath), "estado")
	// Um arquivo no lugar da pasta: não dá para criar nada dentro.
	m.cfg.StateDir = filepath.Join(t.TempDir(), "ocupada")
	if err := os.WriteFile(m.cfg.StateDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.semear(25)
	m.rodarOK()
	if _, err := os.Stat(arquivoDoEstado(m.cfg, alternativa)); err != nil {
		t.Fatalf("o estado devia estar na pasta da configuração: %v", err)
	}
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("%+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
}

// Dois arquivos de estado (a pasta principal ficou sem escrita por um tempo):
// a baixa que chega durante um backup era aplicada só ao arquivo aberto, e a
// fila era zerada — o outro seguia afirmando a versão que saiu do bucket.
func TestBaixaChegaATodosOsArquivosDeEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.ConfigPath = filepath.Join(t.TempDir(), "agent.env")
	alternativa := filepath.Join(filepath.Dir(m.cfg.ConfigPath), "estado")
	m.semear(25)
	m.rodarOK()
	// Uma cópia do estado na outra pasta, como ficaria depois de um período
	// com a principal sem escrita.
	principal := arquivoDoEstado(m.cfg, m.cfg.StateDir)
	copia, err := os.ReadFile(principal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(alternativa, 0o700); err != nil {
		t.Fatal(err)
	}
	outro := arquivoDoEstado(m.cfg, alternativa)
	if err := os.WriteFile(outro, copia, 0o600); err != nil {
		t.Fatal(err)
	}

	k := m.chave("p06/arquivo-00006.txt")
	v, _ := m.s3.Atual(k)
	m.s3.ApagarVersao(k, v.ID)
	// A baixa chega com o backup em curso: espera o estado fechar.
	baixasDoEstado.mu.Lock()
	baixasDoEstado.abertos++
	baixasDoEstado.mu.Unlock()
	darBaixaNoEstado(m.cfg, []estado.VersaoApagada{{Chave: k, Versao: v.ID}})
	baixasDoEstado.mu.Lock()
	baixasDoEstado.abertos--
	baixasDoEstado.mu.Unlock()
	if ver, _ := m.versaoNoEstado(principal, k); ver != v.ID {
		t.Fatalf("com um backup em curso a baixa espera: %q", ver)
	}
	m.rodarOK() // abre o principal, aplica a fila e a zera

	for nome, arq := range map[string]string{"principal": principal, "da configuração": outro} {
		if ver, achou := m.versaoNoEstado(arq, k); achou && ver == v.ID {
			t.Errorf("o estado da pasta %s segue afirmando a versão que recebeu baixa", nome)
		}
	}
}

// A baixa não tem identidade própria (achado B5): o backup abre o estado com
// o bucket do plano, e a baixa o abria com o STORAGE_BUCKET da configuração.
// Quando os dois diferem (instalação antiga sem STORAGE_BUCKET, bucket
// renomeado no painel), a baixa descartava o estado inteiro como "de outro
// armazenamento", e o backup seguinte fazia o mesmo com o que ela recriou.
func TestBaixaNaoDescartaOEstadoQuandoOBucketDaConfiguracaoDifere(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.StorageBucket = "" // instalação de antes do STORAGE_BUCKET
	m.semear(30)
	m.rodarOK()
	k := m.chave("p03/arquivo-00003.txt")
	v, _ := m.s3.Atual(k)
	darBaixaNoEstado(m.cfg, []estado.VersaoApagada{{Chave: k, Versao: v.ID}})
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "delta" || s.Entradas != 1 || m.s3.N("HeadObject") != 1 {
		t.Fatalf("a baixa vale para um arquivo; o resto do estado segue: %+v s3=%v", s, m.s3.Requisicoes())
	}
	m.conferir()
}
