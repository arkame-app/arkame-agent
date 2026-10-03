//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// suffix-version (o padrão) ao lado de um arquivo existente: a cópia fica com
// o modo e o dono dele. Era 0644 root:root — restaurar /etc/shadow no lugar
// deixava /etc/shadow.vXXXX legível por todos ao lado do original 0640.
func TestCopiaComSufixoHerdaModoEDonoDoExistente(t *testing.T) {
	var donos [][2]int
	antes := trocarDono
	t.Cleanup(func() { trocarDono = antes })
	trocarDono = func(_ *os.File, uid, gid int) error { donos = append(donos, [2]int{uid, gid}); return nil }

	conteudo := []byte("root:$6$...:19000::::::")
	dir := t.TempDir()
	existente := filepath.Join(dir, "shadow")
	if err := os.WriteFile(existente, []byte("atual"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existente, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(dir, "shadow", conteudo, "suffix-version")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(dir, "shadow.vv1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != 0o640 {
		t.Fatalf("cópia com sufixo com modo %v, queria 0640 (o do existente)", st.Mode())
	}
	uid, gid := donoDe(t, existente)
	if len(donos) != 1 || donos[0] != [2]int{uid, gid} {
		t.Fatalf("dono da cópia: chamadas %v, queria uma com %d:%d", donos, uid, gid)
	}
}

// A cópia ao lado de um binário setuid/setgid herda as permissões, mas não os
// bits: seria um segundo binário privilegiado, com o conteúdo antigo.
func TestCopiaComSufixoNaoHerdaSetuid(t *testing.T) {
	conteudo := []byte("binário antigo")
	dir := t.TempDir()
	existente := filepath.Join(dir, "su-helper")
	if err := os.WriteFile(existente, []byte("atual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existente, 0o755|os.ModeSetuid|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(dir, "su-helper", conteudo, "suffix-version")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(dir, "su-helper.vv1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != 0o755 {
		t.Fatalf("cópia com sufixo com modo %v, queria 0755 sem setuid/setgid", st.Mode())
	}
}

// Pastas que a restauração cria: 0700. Eram 0755, e restaurar /etc para
// /restore deixava a árvore inteira aberta para listar.
func TestPastasCriadasPelaRestauracaoSao0700(t *testing.T) {
	conteudo := []byte("SENHA=x")
	raiz := t.TempDir()
	item := itemDe(filepath.Join(raiz, "restore"), ".env", conteudo, "suffix-version")
	item.DestFilename = "etc/app/.env"
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"restore", "restore/etc", "restore/etc/app"} {
		st, err := os.Lstat(filepath.Join(raiz, d))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("%s criada com %v, queria 0700", d, st.Mode().Perm())
		}
	}
	st, _ := os.Lstat(filepath.Join(raiz, "restore/etc/app/.env"))
	if st.Mode() != 0o600 {
		t.Fatalf(".env restaurado com %v, queria 0600", st.Mode())
	}
}
