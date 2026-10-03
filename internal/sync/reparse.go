package sync

// Classificação dos reparse points do Windows que o Go entrega como
// fs.ModeIrregular. Fica num arquivo sem build tag, como função pura, para os
// testes rodarem em qualquer sistema; a leitura do atributo e da tag no disco
// está em reparse_windows.go.

// Valores do Windows, repetidos aqui porque golang.org/x/sys/windows só compila
// no Windows. reparse_windows.go confere, na compilação, que batem com os do
// x/sys onde ele os define.
const (
	// IO_REPARSE_TAG_CLOUD e IO_REPARSE_TAG_CLOUD_MASK (ntifs.h; "Reparse Point
	// Tags", learn.microsoft.com/windows-hardware/drivers/ifs/reparse-point-tags
	// e [MS-FSCC] 2.1.2.1). Os arquivos do OneDrive e de outros provedores do
	// Cloud Files API usam IO_REPARSE_TAG_CLOUD_1..F = 0x9000X01A: o nibble
	// X (máscara 0x0000F000) varia, o resto é fixo. x/sys não define estes.
	ioReparseTagCloud     uint32 = 0x9000001A
	ioReparseTagCloudMask uint32 = 0x0000F000

	// Atributos de arquivo ("File Attribute Constants",
	// learn.microsoft.com/windows/win32/fileio/file-attribute-constants).
	attrDirectory          uint32 = 0x00000010
	attrOffline            uint32 = 0x00001000
	attrRecallOnOpen       uint32 = 0x00040000
	attrRecallOnDataAccess uint32 = 0x00400000

	// Tags que não guardam dado do usuário: o atalho de app da loja
	// (IO_REPARSE_TAG_APPEXECLINK), link simbólico e junção. Ficam de fora
	// calados; qualquer outra tag fica de fora, mas conta.
	ioReparseTagAppExecLink uint32 = 0x8000001B
	ioReparseTagSymlink     uint32 = 0xA000000C
	ioReparseTagMountPoint  uint32 = 0xA0000003
)

// classeReparse é o que o walker faz com um reparse point que não é link.
type classeReparse int

const (
	// reparseIgnorar: não guarda dado do usuário (AppExecLink da WindowsApps,
	// link, junção, pasta) ou sumiu durante a leitura. Fica de fora calado.
	reparseIgnorar classeReparse = iota
	// reparseCopiar: arquivo de nuvem com o conteúdo no disco.
	reparseCopiar
	// reparseSoNaNuvem: arquivo de nuvem cujo conteúdo só está no provedor.
	// Abrir baixaria o arquivo; fica de fora e entra só na contagem do log.
	reparseSoNaNuvem
	// reparsePulado: reparse point de outro filtro (Azure File Sync em camada
	// fria, IO_REPARSE_TAG_STORAGE_SYNC 0x8000001E; HSM; WOF) ou que não deu
	// para ler. Fica de fora, mas conta (reparse_skipped no /complete): antes
	// saía calado, a sessão terminava "completa" e o painel lia a falta como
	// arquivo removido na origem.
	reparsePulado
)

// classificarReparse decide pela tag e pelos atributos do próprio reparse
// point (sem segui-lo).
//
// Só os arquivos do Cloud Files API são copiados. Antes, todo reparse point que
// não desse em pasta entrava: os AppExecLinks de
// %LOCALAPPDATA%\Microsoft\WindowsApps\*.exe (tag 0x8000001B) passavam no
// os.Stat, falhavam ao abrir, e todo plano com o perfil do usuário saía
// "parcial", todo dia.
//
// Os demais reparse points que não dão em pasta (Azure File Sync, HSM, WOF,
// filtros de terceiros) voltam reparsePulado: ficam de fora, mas o walker conta.
//
// Arquivo de nuvem só na nuvem (RECALL_ON_DATA_ACCESS, RECALL_ON_OPEN ou
// OFFLINE) fica de fora: lê-lo faria o backup baixar o OneDrive inteiro para o
// disco do cliente. O que já está no disco (inclusive os "sempre manter neste
// dispositivo") entra.
func classificarReparse(atributos, tag uint32) classeReparse {
	if atributos&attrDirectory != 0 {
		return reparseIgnorar
	}
	if tag&^ioReparseTagCloudMask != ioReparseTagCloud {
		switch tag {
		case ioReparseTagAppExecLink, ioReparseTagSymlink, ioReparseTagMountPoint:
			return reparseIgnorar
		}
		return reparsePulado
	}
	if atributos&(attrRecallOnDataAccess|attrRecallOnOpen|attrOffline) != 0 {
		return reparseSoNaNuvem
	}
	return reparseCopiar
}

// lerReparse devolve os atributos e a tag do reparse point em path, sem
// segui-lo. Variável para os testes trocarem.
var lerReparse = lerReparseDoSistema
