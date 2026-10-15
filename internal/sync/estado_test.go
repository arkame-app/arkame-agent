package sync

import (
	"context"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// mesa é um plano de backup de mentira: uma árvore no disco, um bucket falso
// e o estado local, com uma sessão por chamada de rodar. presenca é o que um
// painel teria como "arquivos presentes na última sessão" aplicando o índice
// que o engine emitiu (full substitui, delta aplica).
type mesa struct {
	t        *testing.T
	raiz     string // árvore do cliente
	dirEst   string // pasta do estado
	s3       *bancada.S3Falso
	cli      *s3.Client
	agora    time.Time
	sessao   int
	presenca map[string]api.FileEntry
	// opções da próxima execução
	delta       bool
	rehash      bool
	confirmar   bool
	teto        int
	exclusoes   []string
	semEstado   bool
	trabalhad   int
	ultimaBase  *estado.Base
	confirmadas []int
	// sortear é o sorteio da amostra do estado (nil: o do sistema).
	sortear func(int) int
	// removParcial: o "painel" ofereceu `removed_partial` (item 27 do contrato).
	removParcial bool
	// viva é a LeituraViva da execução (o instantâneo do volume ainda existe?).
	viva func() error
	// noDisco é o AindaNoDisco da execução.
	noDisco func(string) bool
}

func (m *mesa) ultimaConfirmada() int {
	if len(m.confirmadas) == 0 {
		return 0
	}
	return m.confirmadas[len(m.confirmadas)-1]
}

const prefixoDeTeste = "data/ag1/"

func novaMesa(t *testing.T) *mesa {
	t.Helper()
	falso := bancada.NovoS3Falso()
	srv := httptest.NewServer(falso)
	t.Cleanup(srv.Close)
	return &mesa{
		t: t, raiz: t.TempDir(), dirEst: t.TempDir(), s3: falso, cli: bancada.ClienteS3(srv.URL),
		agora: time.Now().Add(time.Hour), presenca: map[string]api.FileEntry{},
		delta: true, confirmar: true, trabalhad: 4,
	}
}

func (m *mesa) escrever(rel, conteudo string) {
	m.t.Helper()
	p := filepath.Join(m.raiz, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(conteudo), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mesa) semear(n int) {
	for i := 0; i < n; i++ {
		m.escrever(fmt.Sprintf("p%02d/arquivo-%04d.txt", i%7, i), fmt.Sprintf("conteúdo %d", i))
	}
}

func (m *mesa) chave(rel string) string {
	return prefixoDeTeste + strings.TrimPrefix(filepath.ToSlash(filepath.Join(m.raiz, rel)), "/")
}

// rodar executa uma sessão e devolve o resultado e o que foi emitido. A
// primeira sessão (sem base) é full; as seguintes, delta, salvo m.delta=false.
func (m *mesa) rodar() (*Result, []api.IndexEntry) {
	m.t.Helper()
	m.sessao++
	sid := fmt.Sprintf("s%d", m.sessao)
	o := EngineOptions{
		S3: m.cli, Bucket: "b", PrefixRoot: prefixoDeTeste, HostRoot: "/",
		SourcePaths: []string{m.raiz}, ExcludeGlobs: m.exclusoes,
		Trabalhadores: m.trabalhad, RehashTotal: m.rehash, VerificarTeto: m.teto,
		Ignorar:      []string{m.dirEst},
		Agora:        func() time.Time { return m.agora },
		Sortear:      m.sortear,
		LeituraViva:  m.viva,
		AindaNoDisco: m.noDisco,
	}
	var emitidas []api.IndexEntry
	o.Emitir = func(e api.IndexEntry) error { emitidas = append(emitidas, e); return nil }

	var plano *estado.Plano
	var ex *estado.Execucao
	delta := false
	if !m.semEstado {
		base, err := estado.Abrir(m.dirEst, estado.Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "b"})
		if err != nil {
			m.t.Fatal(err)
		}
		defer base.Fechar()
		m.ultimaBase = base
		plano, err = base.Plano("p1", PrefixoDasChaves(prefixoDeTeste))
		if err != nil {
			m.t.Fatal(err)
		}
		// Como o daemon: delta só quando o estado está na mesma sessão que o
		// "painel" tem por última concluída.
		delta = m.delta && plano.UltimaSessao() != "" && plano.UltimaSessao() == fmt.Sprintf("s%d", m.ultimaConfirmada())
		ex, err = plano.Comecar(sid)
		if err != nil {
			m.t.Fatal(err)
		}
		o.Estado = ex
		o.Delta = delta
		o.RemovidosEmParcial = delta && m.removParcial
	}
	r, err := Run(context.Background(), o)
	if err != nil && r == nil {
		m.t.Fatal(err)
	}
	if m.confirmar {
		if delta {
			for _, e := range emitidas {
				if e.Op == api.OpUpsert {
					m.presenca[e.Key] = api.FileEntry{Key: e.Key, VersionID: e.VersionID, Size: *e.Size, SHA256: e.SHA256, ModifiedAt: *e.ModifiedAt}
				} else {
					if _, tem := m.presenca[e.Key]; !tem {
						m.t.Errorf("%s de chave que não estava na base: %s", e.Op, e.Key)
					}
					delete(m.presenca, e.Key)
				}
			}
		} else {
			nova := map[string]api.FileEntry{}
			if !r.InventarioCompleto {
				nova = m.presenca
			}
			for _, e := range emitidas {
				if e.Op != api.OpUpsert {
					m.t.Errorf("sessão full emitiu %s", e.Op)
					continue
				}
				nova[e.Key] = api.FileEntry{Key: e.Key, VersionID: e.VersionID, Size: *e.Size, SHA256: e.SHA256, ModifiedAt: *e.ModifiedAt}
			}
			m.presenca = nova
		}
		m.confirmadas = append(m.confirmadas, m.sessao)
		if plano != nil && !r.EstadoFalhou {
			if err := plano.Confirmar(sid, r.Stats.FilesTotal, ex.Ger); err != nil {
				m.t.Fatal(err)
			}
		}
	}
	return r, emitidas
}

// conferir compara a presença com o disco e com o bucket: cada arquivo da
// árvore (fora os de fora) tem de estar presente com o sha256 do conteúdo de
// agora e com a versão atual do bucket, e nada além deles.
func (m *mesa) conferir(fora ...string) {
	m.t.Helper()
	n := 0
	_ = filepath.WalkDir(m.raiz, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, f := range fora {
			if strings.Contains(p, f) {
				return nil
			}
		}
		n++
		b, _ := os.ReadFile(p)
		k := prefixoDeTeste + strings.TrimPrefix(filepath.ToSlash(p), "/")
		e, tem := m.presenca[k]
		if !tem {
			m.t.Errorf("arquivo fora do índice: %s", k)
			return nil
		}
		if e.SHA256 != sha256Hex(b) {
			m.t.Errorf("índice com sha256 de outro conteúdo: %s", k)
		}
		atual, ok := m.s3.Atual(k)
		if !ok || atual.ID != e.VersionID || atual.SHA256 != e.SHA256 {
			m.t.Errorf("índice aponta %s para a versão %q; a atual do bucket é %+v (existe=%v)", k, e.VersionID, atual, ok)
		}
		return nil
	})
	if len(m.presenca) != n {
		m.t.Errorf("índice com %d arquivos, disco com %d", len(m.presenca), n)
	}
}
