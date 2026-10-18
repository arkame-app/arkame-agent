package atualizacao

import "testing"

// O irmão que segue o principal não se atualiza sozinho, mas é reconhecido.
func TestSegueEDesligada(t *testing.T) {
	if !Desligada("follow") || !Segue("follow") || Segue("off") || Segue("") {
		t.Fatal("follow tem de desligar a atualização própria e marcar que segue o principal")
	}
}
