package estado

import (
	"syscall"

	bolt "go.etcd.io/bbolt"
)

// aliviar devolve ao sistema as páginas do arquivo de estado que este
// processo tem mapeadas.
//
// O bbolt lê o arquivo por um mapeamento de memória, e cada página tocada
// passa a contar na memória residente do processo. Uma execução sem mudança
// consulta todas as entradas: com 1 milhão de arquivos o estado tem ~250 MB, e
// era esse o tamanho que o agente aparentava ocupar — num container com
// limite de memória, é o que o limite conta. O madvise(MADV_DONTNEED) num
// mapeamento de arquivo compartilhado e só de leitura (o bbolt grava pelo
// descritor, não pelo mapa) só desfaz o vínculo das páginas com o processo:
// os dados seguem no cache do sistema, e a próxima leitura as traz de volta
// sem ir ao disco.
//
// Roda dentro de uma transação de leitura: enquanto ela vive o bbolt não
// refaz o mapeamento, então o endereço e o tamanho valem.
func (b *Base) aliviar() {
	if b.morta.Load() {
		return
	}
	_ = b.protegido(func() error {
		return b.db.View(func(tx *bolt.Tx) error {
			inicio, tamanho := b.db.Info().Data, tx.Size()
			if inicio == 0 || tamanho <= 0 {
				return nil
			}
			_, _, _ = syscall.Syscall(syscall.SYS_MADVISE, inicio, uintptr(tamanho), syscall.MADV_DONTNEED)
			return nil
		})
	})
}
