//go:build windows && arm64

package instantaneo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Os métodos que recebem um VSS_ID (GUID de 16 bytes) por valor. No ARM64, um
// composto de até 16 bytes vai em dois registradores seguidos.

func metades(g *windows.GUID) (uintptr, uintptr) {
	w := *(*[2]uint64)(unsafe.Pointer(g))
	return uintptr(w[0]), uintptr(w[1])
}

func isVolumeSupported(bc *objetoCOM, provedor *windows.GUID, nome *uint16, suportado *int32) uint32 {
	a, b := metades(provedor)
	return bc.chamar(mIsVolumeSupported, a, b, uintptr(unsafe.Pointer(nome)), uintptr(unsafe.Pointer(suportado)))
}

func addToSnapshotSet(bc *objetoCOM, nome *uint16, provedor *windows.GUID, id *windows.GUID) uint32 {
	a, b := metades(provedor)
	return bc.chamar(mAddToSnapshotSet, uintptr(unsafe.Pointer(nome)), a, b, uintptr(unsafe.Pointer(id)))
}

func getSnapshotProperties(bc *objetoCOM, id *windows.GUID, p *propriedades) uint32 {
	a, b := metades(id)
	return bc.chamar(mGetSnapshotProperties, a, b, uintptr(unsafe.Pointer(p)))
}
