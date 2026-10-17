package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestAvisoDoDiscoMostraOEspaco(t *testing.T) {
	var out bytes.Buffer
	avisarDoDisco(t.TempDir(), &out)
	if !strings.Contains(out.String(), "Espaço livre no disco de") {
		t.Fatalf("não mostrou o espaço livre: %q", out.String())
	}
	if len(pontosDeMontagem()) == 0 {
		t.Skip("sem pontos de montagem de disco neste ambiente")
	}
}
