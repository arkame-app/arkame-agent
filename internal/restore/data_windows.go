//go:build windows

package restore

import (
	"time"

	"golang.org/x/sys/windows"
)

// definirData grava a data de modificação (e a de acesso) do arquivo do
// handle (aberto com FILE_WRITE_ATTRIBUTES).
//
// Não é o os.Chtimes: ele converte por UnixNano, que só cobre 1677–2262, e a
// data restaurada de um arquivo fora disso dava a volta. O FILETIME (intervalos
// de 100 ns desde 1601) vai até o ano 30828 e é montado aqui sem passar por
// UnixNano.
func definirData(h windows.Handle, acesso, modificacao time.Time) error {
	a, m := filetime(acesso), filetime(modificacao)
	return windows.SetFileTime(h, nil, &a, &m)
}

// segundosDe1601Ate1970 separa a época do FILETIME da do Unix.
const segundosDe1601Ate1970 = 11644473600

func filetime(t time.Time) windows.Filetime {
	n := (t.Unix()+segundosDe1601Ate1970)*10_000_000 + int64(t.Nanosecond())/100
	if n < 0 {
		n = 0
	}
	return windows.Filetime{LowDateTime: uint32(n), HighDateTime: uint32(n >> 32)}
}
