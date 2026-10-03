package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pasta vazia: o painel precisa ver "entries": [] — sem o campo, o explorador
// de pastas ficava esperando uma listagem que já tinha chegado.
func TestListagemDePastaVaziaLevaEntries(t *testing.T) {
	b, err := json.Marshal(FsListingReport{Entries: []FsEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"entries":[]`) {
		t.Fatalf("pasta vazia serializada como %s", b)
	}
}
