//go:build windows && amd64

package instantaneo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Os métodos que recebem um VSS_ID (GUID de 16 bytes) por valor. No x64 da
// Microsoft, o que não tem 1, 2, 4 ou 8 bytes vai como ponteiro para uma cópia
// alinhada em 16 — o GUID alocado com new() está (classe de tamanho 16 do
// alocador do Go).

func isVolumeSupported(bc *objetoCOM, provedor *windows.GUID, nome *uint16, suportado *int32) uint32 {
	return bc.chamar(mIsVolumeSupported, uintptr(unsafe.Pointer(provedor)), uintptr(unsafe.Pointer(nome)), uintptr(unsafe.Pointer(suportado)))
}

func addToSnapshotSet(bc *objetoCOM, nome *uint16, provedor *windows.GUID, id *windows.GUID) uint32 {
	return bc.chamar(mAddToSnapshotSet, uintptr(unsafe.Pointer(nome)), uintptr(unsafe.Pointer(provedor)), uintptr(unsafe.Pointer(id)))
}

func getSnapshotProperties(bc *objetoCOM, id *windows.GUID, p *propriedades) uint32 {
	return bc.chamar(mGetSnapshotProperties, uintptr(unsafe.Pointer(id)), uintptr(unsafe.Pointer(p)))
}
