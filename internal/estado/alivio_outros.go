//go:build !linux

package estado

// aliviar: fora do Linux as páginas do arquivo de estado ficam mapeadas até o
// fim da execução (o sistema as recolhe sob pressão de memória).
func (b *Base) aliviar() {}
