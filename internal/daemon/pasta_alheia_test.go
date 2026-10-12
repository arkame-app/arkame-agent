package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/arkame-app/agent/internal/segredo"
)

// donoAlheio faz os caminhos dados parecerem de outro usuário, como a pasta
// que alguém deixou pronta antes de o agente ser instalado. dentro faz valer
// também para o que estiver abaixo deles.
func donoAlheio(t *testing.T, caminhos ...string) {
	t.Helper()
	antesPasta, antesDono := pastaPrivada, conferirDono
	t.Cleanup(func() { pastaPrivada, conferirDono = antesPasta, antesDono })
	alheio := func(p string) bool {
		for _, c := range caminhos {
			if filepath.Clean(p) == filepath.Clean(c) {
				return true
			}
		}
		return false
	}
	conferirDono = func(p string) error {
		if alheio(p) {
			return fmt.Errorf("%w: %s pertence a uid 4242", segredo.ErrDonoNaoConfiavel, p)
		}
		return antesDono(p)
	}
	pastaPrivada = func(dir string) error {
		if alheio(dir) {
			return fmt.Errorf("%w: %s pertence a uid 4242", segredo.ErrDonoNaoConfiavel, dir)
		}
		return antesPasta(dir)
	}
}

func arquivosEm(t *testing.T, dir string) []string {
	t.Helper()
	itens, _ := os.ReadDir(dir)
	var nomes []string
	for _, i := range itens {
		nomes = append(nomes, i.Name())
	}
	return nomes
}

// O achado A3 da revisão: a pasta do estado já existe, criada por outro
// usuário (no Windows, qualquer um cria %ProgramData%\Arkame antes de o agente
// ser instalado e fica dono dela). O agente, como SYSTEM, gravava o estado lá
// — onde o dono lê os nomes de todos os arquivos e planta entradas. A pasta é
// recusada, nada é gravado nela, e o estado vai para a da configuração.
func TestPastaDoEstadoDeOutroDonoNaoEUsada(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.ConfigPath = filepath.Join(t.TempDir(), "agent.env")
	alternativa := filepath.Join(filepath.Dir(m.cfg.ConfigPath), "estado")
	donoAlheio(t, m.cfg.StateDir)
	m.semear(30)
	m.rodarOK()
	if nomes := arquivosEm(t, m.cfg.StateDir); len(nomes) != 0 {
		t.Fatalf("o agente gravou na pasta de outro dono: %v", nomes)
	}
	if _, err := os.Stat(arquivoDoEstado(m.cfg, alternativa)); err != nil {
		t.Fatalf("o estado devia ter ido para a pasta da configuração: %v", err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(alternativa); st.Mode().Perm() != 0o700 {
			t.Fatalf("a pasta alternativa nasce fechada: %v", st.Mode().Perm())
		}
	}
	// E é usada daí em diante.
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("%+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	if nomes := arquivosEm(t, m.cfg.StateDir); len(nomes) != 0 {
		t.Fatalf("o agente gravou na pasta de outro dono: %v", nomes)
	}
}

// As duas pastas de outro dono: o backup roda sem estado — lê e confere tudo,
// como a v0.4.31 — e não grava nada em nenhuma (nem o temporário do índice).
func TestSemPastaConfiavelOBackupRodaSemEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.ConfigPath = filepath.Join(t.TempDir(), "agent.env")
	alternativa := filepath.Join(filepath.Dir(m.cfg.ConfigPath), "estado")
	donoAlheio(t, m.cfg.StateDir, alternativa)
	m.semear(30)
	m.rodarOK()
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Status != "complete" || s.Modo != "full" || m.s3.N("HeadObject") != 30 || m.s3.N("PutObject") != 0 {
		t.Fatalf("%+v s3=%v", s, m.s3.Requisicoes())
	}
	if nomes := arquivosEm(t, m.cfg.StateDir); len(nomes) != 0 {
		t.Fatalf("o agente gravou na pasta de outro dono: %v", nomes)
	}
	if _, err := os.Stat(alternativa); err == nil {
		t.Fatal("a pasta alternativa, de outro dono, não devia nem ser criada")
	}
	m.conferir()
}

// Arquivo de estado posto por outro usuário numa pasta que é do administrador
// mas tinha a permissão aberta (a pasta criada por um agente de antes desta
// correção, no Windows): é descartado, e o backup confere tudo no bucket.
func TestArquivoDeEstadoDeOutroDonoEDescartado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(30)
	m.rodarOK()
	plantado := arquivoDoEstado(m.cfg, m.cfg.StateDir)
	donoAlheio(t, plantado)
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "full" || s.Entradas != 30 || m.s3.N("HeadObject") != 30 || m.s3.N("HeadObjectVersion") != 0 {
		t.Fatalf("o estado de outro dono não pode servir de atalho: %+v s3=%v", s, m.s3.Requisicoes())
	}
	m.conferir()
}
