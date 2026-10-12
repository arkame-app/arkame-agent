package daemon

import (
	"os"
	"testing"

	"github.com/arkame-app/agent/internal/estado"
)

// arquivoDoEstado é o arquivo de estado da mesa.
func (m *mesaDoDaemon) arquivoDoEstado() string {
	return estado.NomeDoArquivo(m.cfg.StateDir, identidadeDoEstado(m.cfg, m.plano.StorageRef.Bucket))
}

// Estado de outra época (achado M1 da revisão): uma cópia antiga do arquivo de
// estado volta ao lugar — rollback de snapshot, VM clonada — depois de o
// painel já ter concluído outra sessão. O delta já era desligado, mas as
// entradas seguiam servindo de atalho: o inventário saía do estado antigo, sem
// conferir no bucket, e o catálogo afirmava uma versão que não existe mais.
func TestEstadoDeOutraEpocaNaoServeDeAtalho(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(200)
	m.rodarOK()
	antigo, err := os.ReadFile(m.arquivoDoEstado())
	if err != nil {
		t.Fatal(err)
	}
	m.escrever("p00/arquivo-00000.txt", "mudou")
	m.rodarOK() // o painel avança uma sessão
	if err := os.WriteFile(m.arquivoDoEstado(), antigo, 0o600); err != nil {
		t.Fatal(err)
	}
	// E uma versão que o estado antigo afirma saiu do bucket.
	k := m.chave("p05/arquivo-00005.txt")
	v, _ := m.s3.Atual(k)
	m.s3.ApagarVersao(k, v.ID)

	m.s3.Zerar()
	s := m.rodarOK()
	if s.Status != "complete" || s.Modo != "full" || s.Entradas != 200 {
		t.Fatalf("%+v", s)
	}
	if m.s3.N("HeadObject") != 200 || m.s3.N("HeadObjectVersion") != 0 {
		t.Fatalf("estado de outra época: os 200 arquivos têm de ser conferidos no bucket, um a um: %v", m.s3.Requisicoes())
	}
	if m.s3.N("PutObject") != 1 {
		t.Fatalf("só o arquivo cuja versão sumiu sobe de novo: %v", m.s3.Requisicoes())
	}
	m.conferir()
	// Realinhado, o estado volta a valer.
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("seguinte: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
}
