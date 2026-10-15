package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/segredo"
)

// newPrepareDumpFolderCmd prepara a pasta do dump de um banco antes do backup.
// Feito para o comando de antes do plano, que o chama pela variável
// ARKAME_AGENT (o caminho deste programa):
//
//	"%ARKAME_AGENT%" prepare-dump-folder C:\ProgramData\Arkame\dumps\sqlserver --writer "NT Service\MSSQLSERVER"
//
// Falha (e o backup é cancelado) se a pasta, ou algo dentro dela, foi criado
// por outra pessoa: segredo.PastaDeDump.
func newPrepareDumpFolderCmd() *cobra.Command {
	var escritor string
	cmd := &cobra.Command{
		Use:   "prepare-dump-folder <pasta>",
		Short: "Cria a pasta do dump de um banco, legível só pelo administrador (para o comando de antes do backup)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := segredo.PastaDeDump(args[0], escritor); err != nil {
				return fmt.Errorf("pasta do dump: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&escritor, "writer", "", `conta que grava o dump, além do agente (Windows; ex.: "NT Service\MSSQLSERVER")`)
	return cmd
}
