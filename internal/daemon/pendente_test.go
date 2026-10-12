package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	syncengine "github.com/arkame-app/agent/internal/sync"
)

// Sessão que o agente decidiu não confirmar (achado M2 da revisão): inventário
// de sessão parcial em que o estado local falhou no meio. O /complete fecha
// com 40 dos 45 arquivos e o estado não é confirmado — mas na execução
// seguinte o painel devolvia essa sessão como base, e o agente a adotava como
// se só a resposta tivesse se perdido: delta de zero entradas, e os 5 arquivos
// ficavam fora do catálogo.
func TestSessaoQueOAgenteNaoConfirmouNaoEAdotada(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de uma pasta sem permissão de leitura")
	}
	m := novaMesaDoDaemon(t)
	m.semear(45)
	m.rodarOK()

	m.painel.SemDelta = true
	trancada := filepath.Join(m.raiz, "p03")
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })
	deVerdade := rodarSync
	t.Cleanup(func() { rodarSync = deVerdade })
	rodarSync = func(ctx context.Context, o syncengine.EngineOptions) (*syncengine.Result, error) {
		r, err := deVerdade(ctx, o)
		if r != nil {
			r.EstadoFalhou = true // disco cheio no meio da gravação do estado
		}
		return r, err
	}
	if s := m.rodarOK(); s.Status != "partial" || s.Modo != "full" || s.Presentes != 40 {
		t.Fatalf("full parcial: %+v", s)
	}

	rodarSync = deVerdade
	_ = os.Chmod(trancada, 0o755)
	m.painel.SemDelta = false
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Status != "complete" || s.Presentes != 45 {
		t.Fatalf("a sessão não confirmada foi adotada: os 5 arquivos ficaram fora do catálogo: %+v", s)
	}
	if m.s3.N("PutObject") != 0 {
		t.Fatalf("nada precisa subir de novo: %v", m.s3.Requisicoes())
	}
	m.conferir()
}

// Índice refeito numa sessão nova (achado B9): a geração do estado vai ao
// painel com o id da sessão nova. Se a resposta do /complete dela se perde, é
// esse id que o painel devolve como base — e o estado tem de reconhecê-lo. A
// pendente continuava sendo a sessão recusada, e o backup seguinte relia tudo.
func TestSessaoRefeitaComRespostaPerdidaEConfirmadaPelaBase(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(60)
	m.rodarOK()
	m.escrever("p00/arquivo-00000.txt", "mudou")
	m.painel.ForcarBaseErrada(1)
	var perder atomic.Bool
	perder.Store(true)
	m.painel.Engolir = func(rota string, corpo []byte) bool {
		return rota == "complete" && perder.Load() && bytes.Contains(corpo, []byte(`"mode":"full"`))
	}
	if err := m.rodar(); err == nil {
		t.Fatal("sem resposta do /complete o backup devolve erro")
	}
	ss := m.painel.Sessoes()
	if len(ss) != 3 || ss[1].Status != "failed" || ss[2].Status != "complete" || ss[2].Modo != "full" {
		t.Fatalf("queria a sessão do delta recusada e a refeita concluída no painel: %d sessões, %+v", len(ss), ss[len(ss)-1])
	}
	perder.Store(false)
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("a sessão refeita devia ser reconhecida como a base: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	m.conferir()
}
