package sync

import (
	"testing"

	"github.com/arkame-app/agent/internal/caminho"
)

// No Windows (achado B3), a pasta do estado escrita com outra caixa ou outra
// barra é a mesma pasta: a varredura não pode entrar nela só porque o plano
// diz `c:\programdata` e a configuração, `C:\ProgramData`.
func TestPastaIgnoradaNoWindowsNaoDependeDaCaixa(t *testing.T) {
	antes := caminho.Windows
	t.Cleanup(func() { caminho.Windows = antes })

	estado := []string{`C:\ProgramData\Arkame\estado`}
	caminho.Windows = true
	for _, p := range []string{
		`C:\ProgramData\Arkame\estado`,
		`c:\programdata\arkame\estado`,
		`C:/ProgramData/Arkame/estado`,
		`C:\PROGRAMDATA\ARKAME\ESTADO\`,
	} {
		if !pastaIgnorada(p, estado) {
			t.Errorf("no Windows %q é a pasta do estado e tinha de ficar fora", p)
		}
	}
	for _, p := range []string{`C:\ProgramData\Arkame`, `C:\ProgramData\Arkame\estado2`, `D:\ProgramData\Arkame\estado`} {
		if pastaIgnorada(p, estado) {
			t.Errorf("%q não é a pasta do estado", p)
		}
	}
	if pastaIgnorada(`C:\`, []string{""}) {
		t.Error("entrada vazia não ignora nada")
	}

	// Fora do Windows a caixa conta: /var/lib/Arkame não é /var/lib/arkame.
	caminho.Windows = false
	if !pastaIgnorada("/var/lib/arkame/estado", []string{"/var/lib/arkame/estado"}) ||
		pastaIgnorada("/var/lib/Arkame/estado", []string{"/var/lib/arkame/estado"}) {
		t.Error("fora do Windows a comparação é exata")
	}
}
