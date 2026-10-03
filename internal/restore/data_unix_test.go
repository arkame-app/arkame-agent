//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// O os.Chtimes converte por UnixNano (1677–2262): a data do backup de um
// arquivo do ano 9999 era gravada dando a volta.
func TestRestauracaoComDataForaDoUnixNano(t *testing.T) {
	dir := t.TempDir()
	quando := time.Date(9999, 6, 1, 12, 0, 0, 0, time.UTC)

	// O sistema de arquivos guarda o ano 9999? (ext4 para em 2446.)
	sonda := filepath.Join(dir, "sonda")
	if err := os.WriteFile(sonda, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := unix.TimeToTimespec(quando)
	if err != nil {
		t.Skipf("plataforma sem timespec de 64 bits: %v", err)
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, sonda, []unix.Timespec{ts, ts}, 0); err != nil {
		t.Skipf("o sistema de arquivos não aceitou a data: %v", err)
	}
	if st, _ := os.Stat(sonda); !st.ModTime().Equal(quando) {
		t.Skip("o sistema de arquivos não guarda o ano 9999")
	}

	conteudo := []byte("x")
	item := itemDe(dir, "futuro.txt", conteudo, "suffix-version")
	item.SourceModifiedAt = &quando
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "futuro.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(quando) {
		t.Fatalf("mtime = %v, queria %v", st.ModTime(), quando)
	}
}
