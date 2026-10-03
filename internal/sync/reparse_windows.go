//go:build windows

package sync

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Os valores de reparse.go têm de bater com os do x/sys onde ele os define: um
// array de tamanho diferente de zero não cabe em [0]struct{}, e a compilação
// para Windows quebra se alguém errar um número.
var (
	_ [0]struct{} = [attrDirectory ^ windows.FILE_ATTRIBUTE_DIRECTORY]struct{}{}
	_ [0]struct{} = [attrOffline ^ windows.FILE_ATTRIBUTE_OFFLINE]struct{}{}
	_ [0]struct{} = [attrRecallOnOpen ^ windows.FILE_ATTRIBUTE_RECALL_ON_OPEN]struct{}{}
	_ [0]struct{} = [attrRecallOnDataAccess ^ windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS]struct{}{}
)

// fileAttributeTagInfo é FILE_ATTRIBUTE_TAG_INFO (winbase.h), que o x/sys não
// define.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// lerReparseDoSistema abre o próprio reparse point (FILE_FLAG_OPEN_REPARSE_POINT,
// só com FILE_READ_ATTRIBUTES: não segue o link nem baixa o conteúdo de um
// arquivo do OneDrive) e lê atributos e tag com FileAttributeTagInfo.
func lerReparseDoSistema(path string) (atributos, tag uint32, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	var info fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, 0, err
	}
	return info.FileAttributes, info.ReparseTag, nil
}
