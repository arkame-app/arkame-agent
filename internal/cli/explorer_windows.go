package cli

import (
	"github.com/inconshreveable/mousetrap"
	"github.com/spf13/cobra"
)

func init() {
	// Sem a trava do cobra ("This is a command line tool…" por 5 segundos):
	// o duplo clique no instalador vira o setup interativo (Execute).
	cobra.MousetrapHelpText = ""
}

// abertoPeloExplorer: o programa foi aberto com dois cliques (o pai é o Explorer).
func abertoPeloExplorer() bool { return mousetrap.StartedByExplorer() }
