//go:build windows

package segredo

// sincronizarPastaSO não faz nada no Windows: lá não se abre uma pasta para
// FlushFileBuffers, e o NTFS registra o rename no journal dele.
func sincronizarPastaSO(string) error { return nil }
