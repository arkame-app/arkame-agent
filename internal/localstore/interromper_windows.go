//go:build windows

package localstore

import "os"

// interromper: no Windows não há sinal para um processo de console sem
// janela; o RustFS é encerrado direto. Os objetos já gravados ficam; a escrita
// em curso é a que o agente vai repetir.
func interromper(p *os.Process) error { return p.Kill() }
