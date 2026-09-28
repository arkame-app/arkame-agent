package cli

import (
	"fmt"
	"os"

	"github.com/arkame-app/agent/internal/terminal"
)

// esperarEnter segura a janela aberta pelo Windows (Executar, Aplicativos
// instalados) até a pessoa ler o resultado.
func esperarEnter(err *error) {
	if *err != nil {
		fmt.Fprintln(os.Stderr, "\n  ✗", *err)
	}
	fmt.Fprint(os.Stderr, "\n  Pressione Enter para fechar.")
	if t, terr := terminal.Open(); terr == nil {
		_, _ = t.Pergunta("")
		t.Close()
	}
}
