// Command arkame-agent é o agent cliente do Arkame: lê arquivos do servidor
// e envia para o bucket do cliente (BYOS), reportando status ao painel Arkame.
//
// Subcomandos:
//   - install   Registra o agente no painel (first-time ou re-enrollment) e opcionalmente instala como serviço do SO
//   - run       Inicia o loop daemon (enrollment já concluído)
//   - status    Mostra estado local (fingerprint, último heartbeat, plans ativos)
//   - version   Imprime build info
package main

import (
	"fmt"
	"os"

	"github.com/arkame-app/agent/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
