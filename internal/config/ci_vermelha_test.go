package config

import "testing"

// Falha PROPOSITAL, combinada com o fundador em 24/09, para provar que o `main`
// vermelho abre uma issue e que o aviso chega. É revertida no commit seguinte.
func TestFalhaPropositalDoAvisoDeCI(t *testing.T) {
	t.Fatal("falha proposital: prova do aviso de CI vermelha — será revertida")
}
