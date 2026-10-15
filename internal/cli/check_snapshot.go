package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/caminho"
	"github.com/arkame-app/agent/internal/instantaneo"
)

// newCheckSnapshotCmd testa o instantâneo do volume (VSS) nesta máquina: cria,
// lista a pasta dentro dele, lê um arquivo e solta. É a prova de campo do que
// o backup faz antes de ler — sem ela, a primeira notícia de um VSS que não
// funciona seria o aviso no painel depois do backup.
func newCheckSnapshotCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `check-snapshot [pasta]`,
		Short: `Testa o instantâneo do volume (VSS) que o backup usa no Windows (padrão: C:\Users)`,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !caminho.Windows {
				return errors.New("o instantâneo do volume só existe no Windows; nos outros sistemas o backup lê do disco")
			}
			pasta := `C:\Users`
			if len(args) == 1 {
				pasta = args[0]
			}
			saida := cmd.ErrOrStderr()
			pasta = caminho.NoDisco("", pasta)
			inst := instantaneo.Abrir(cmd.Context(), "auto", []string{pasta})
			defer inst.Fechar(cmd.Context())
			if !inst.Ativo() {
				return fmt.Errorf("✗ %s", inst.Aviso)
			}
			dentro := inst.Ler(pasta)
			fmt.Fprintln(saida, "  ✓ Instantâneo criado:", dentro)
			itens, err := os.ReadDir(dentro)
			if err != nil {
				return fmt.Errorf("✗ o instantâneo foi criado, mas a pasta não abriu dentro dele: %w", err)
			}
			fmt.Fprintf(saida, "  ✓ %d itens em %s, lidos do instantâneo\n", len(itens), pasta)
			for _, it := range itens {
				if !it.Type().IsRegular() {
					continue
				}
				n, err := lerTudo(filepath.Join(dentro, it.Name()))
				if err != nil {
					fmt.Fprintf(saida, "  ! %s não abriu dentro do instantâneo: %v\n", it.Name(), err)
					continue
				}
				fmt.Fprintf(saida, "  ✓ %s lido inteiro do instantâneo (%d bytes)\n", it.Name(), n)
				break
			}
			inst.Fechar(cmd.Context())
			fmt.Fprintln(saida, "  ✓ Instantâneo solto. O backup deste servidor lê os arquivos assim.")
			return nil
		},
	}
}

func lerTudo(p string) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(io.Discard, f)
}
