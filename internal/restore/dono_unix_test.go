//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func donoDe(t *testing.T, p string) (int, int) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sys().(*syscall.Stat_t)
	return int(s.Uid), int(s.Gid)
}

// chownQueLimpaSetuid troca o copiarDono por um que faz o que o chown do Linux
// faz com o arquivo, mesmo pelo root: limpa S_ISUID e S_ISGID. Assim o teste
// pega, sem root, um chmod feito antes do chown. Devolve a ordem das chamadas.
func chownQueLimpaSetuid(t *testing.T) *[]string {
	t.Helper()
	var chamadas []string
	antes := trocarDono
	t.Cleanup(func() { trocarDono = antes })
	trocarDono = func(f *os.File, _ os.FileInfo) error {
		st, err := f.Stat()
		if err != nil {
			return err
		}
		chamadas = append(chamadas, "chown modo="+st.Mode().String())
		return f.Chmod(st.Mode() &^ (os.ModeSetuid | os.ModeSetgid))
	}
	return &chamadas
}

// Binário setuid/setgid restaurado por cima do original fica setuid/setgid.
// Antes o chmod vinha antes do chown, e o chown apagava os bits.
func TestRestauracaoPreservaSetuidDepoisDoChown(t *testing.T) {
	chamadas := chownQueLimpaSetuid(t)
	conteudo := []byte("binário do backup")
	dir := t.TempDir()
	final := filepath.Join(dir, "su-helper")
	if err := os.WriteFile(final, []byte("velho"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := os.FileMode(0o755) | os.ModeSetuid | os.ModeSetgid
	if err := os.Chmod(final, original); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, itemDe(dir, "su-helper", conteudo, "overwrite")); err != nil {
		t.Fatal(err)
	}
	if len(*chamadas) != 1 {
		t.Fatalf("chown chamado %d vezes, queria 1: %v", len(*chamadas), *chamadas)
	}
	st, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != original {
		t.Fatalf("modo depois da restauração = %v, queria %v (o chmod tem de vir depois do chown)", st.Mode(), original)
	}
}

// O mesmo com o chown de verdade: como root, um 04755 de outro dono.
func TestRestauracaoComoRootPreservaSetuid(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("exige root")
	}
	conteudo := []byte("binário do backup")
	dir := t.TempDir()
	final := filepath.Join(dir, "su-helper")
	if err := os.WriteFile(final, []byte("velho"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(final, 1234, 5678); err != nil {
		t.Fatal(err)
	}
	original := os.FileMode(0o755) | os.ModeSetuid
	if err := os.Chmod(final, original); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, itemDe(dir, "su-helper", conteudo, "overwrite")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != original {
		t.Fatalf("modo depois da restauração = %v, queria %v", st.Mode(), original)
	}
	if uid, gid := donoDe(t, final); uid != 1234 || gid != 5678 {
		t.Fatalf("dono %d:%d, queria 1234:5678", uid, gid)
	}
}
