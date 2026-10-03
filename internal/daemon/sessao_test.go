package daemon

import (
	"errors"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// Dia sem mudança (tudo dedup, nada enviado) com uma pasta ilegível: o resto
// está salvo e indexável — é parcial, não falha.
func TestAvaliarSessaoDedupComErroEParcial(t *testing.T) {
	r := &syncengine.Result{VersionMap: []api.FileEntry{{Key: "a"}}}
	falhou, status := avaliarSessao(r, errors.New("1 itens não puderam ser lidos"))
	if falhou || status != "partial" {
		t.Fatalf("esperava partial, veio falhou=%v status=%q", falhou, status)
	}
}

func TestAvaliarSessao(t *testing.T) {
	erro := errors.New("x")
	casos := []struct {
		nome   string
		r      *syncengine.Result
		err    error
		falhou bool
		status string
	}{
		{"sem resultado", nil, erro, true, ""},
		{"erro e nada no mapa", &syncengine.Result{}, erro, true, ""},
		{"uploads falharam todos", &syncengine.Result{FilesFailed: 2}, nil, true, ""},
		{"um falhou, outro foi", &syncengine.Result{FilesFailed: 1, VersionMap: []api.FileEntry{{}}}, nil, false, "partial"},
		{"tudo certo", &syncengine.Result{VersionMap: []api.FileEntry{{}}}, nil, false, "complete"},
		{"plano vazio", &syncengine.Result{}, nil, false, "complete"},
	}
	for _, c := range casos {
		falhou, status := avaliarSessao(c.r, c.err)
		if falhou != c.falhou || status != c.status {
			t.Errorf("%s: veio falhou=%v status=%q", c.nome, falhou, status)
		}
	}
}
