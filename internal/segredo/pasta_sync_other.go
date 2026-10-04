//go:build !windows

package segredo

import "os"

// sincronizarPastaSO grava no disco a entrada da pasta (o rename): sem isso,
// depois de um corte de energia o nome pode voltar a apontar o arquivo antigo,
// ou a nenhum.
func sincronizarPastaSO(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
