package daemon

import (
	"strings"
	"syscall"
	"testing"

	syncengine "github.com/arkame-app/agent/internal/sync"
)

// No macOS, "operation not permitted" é o TCC barrando o serviço sem Acesso
// Total ao Disco. A causa da sessão parcial dizia só o erro, e ninguém sabia o
// que fazer: agora diz o passo. Fora do macOS, ou sem EPERM, nada muda.
func TestCausaDoParcialPedeAcessoTotalAoDiscoNoMacOS(t *testing.T) {
	antes := sistemaDoAgente
	t.Cleanup(func() { sistemaDoAgente = antes })
	barrado := &syncengine.Result{FilesFailed: 1, PrimeiraFalha: "Users/ana/Desktop/a.txt: open: operation not permitted", NaoPermitido: true}
	comum := &syncengine.Result{FilesFailed: 1, PrimeiraFalha: "a.txt: arquivo mudou durante a leitura"}

	sistemaDoAgente = "darwin"
	if m := causaDoParcial(barrado, nil); !strings.Contains(m, "Acesso Total ao Disco") || !strings.Contains(m, "Privacidade e Segurança") {
		t.Fatalf("macOS com EPERM deveria pedir Acesso Total ao Disco: %q", m)
	}
	// EPERM só no erro da varredura (sem a marca do engine) também conta.
	if m := causaDoParcial(&syncengine.Result{}, &errorComCausa{syscall.EPERM}); !strings.Contains(m, "Acesso Total ao Disco") {
		t.Fatalf("EPERM no erro da varredura: %q", m)
	}
	if m := causaDoParcial(comum, nil); strings.Contains(m, "Acesso Total") {
		t.Fatalf("sem EPERM não há dica: %q", m)
	}
	if m := causaDoParcial(&syncengine.Result{NaoPermitido: true}, nil); m != "" {
		t.Fatalf("sem causa, a dica sozinha não vira causa: %q", m)
	}

	sistemaDoAgente = "linux"
	if m := causaDoParcial(barrado, nil); strings.Contains(m, "Acesso Total") {
		t.Fatalf("fora do macOS não há dica: %q", m)
	}
}

type errorComCausa struct{ causa error }

func (e *errorComCausa) Error() string {
	return "não consegui ler /Users/ana/Documents: " + e.causa.Error()
}
func (e *errorComCausa) Unwrap() error { return e.causa }
