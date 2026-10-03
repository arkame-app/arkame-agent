//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// grupoExtra é um grupo deste usuário diferente do principal: a pasta-mãe
// passa a ser dele, e o teste distingue "o dono da pasta" de "quem roda o
// agente" sem root.
func grupoExtra(t *testing.T, pasta string) int {
	t.Helper()
	grupos, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grupos {
		if g != os.Getegid() {
			if err := os.Chown(pasta, -1, g); err != nil {
				t.Fatal(err)
			}
			return g
		}
	}
	t.Skip("o usuário do teste não tem grupo além do principal")
	return 0
}

func registrarDonos(t *testing.T) (arquivos, pastas *[][2]int) {
	t.Helper()
	var a, p [][2]int
	antesA, antesP := trocarDono, trocarDonoDaPasta
	t.Cleanup(func() { trocarDono, trocarDonoDaPasta = antesA, antesP })
	trocarDono = func(_ *os.File, uid, gid int) error { a = append(a, [2]int{uid, gid}); return nil }
	trocarDonoDaPasta = func(_ int, uid, gid int) error { p = append(p, [2]int{uid, gid}); return nil }
	return &a, &p
}

// Arquivo novo (sem um de mesmo nome no destino): fica do dono e do grupo da
// pasta. Do root, saía root:root 0600, e a Ana não abria o próprio arquivo
// restaurado no lugar.
func TestArquivoNovoFicaDoDonoDaPasta(t *testing.T) {
	arquivos, _ := registrarDonos(t)
	conteudo := []byte("relatório")
	dir := t.TempDir()
	g := grupoExtra(t, dir)
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(dir, "relatorio.odt", conteudo, "suffix-version")); err != nil {
		t.Fatal(err)
	}
	if len(*arquivos) != 1 || (*arquivos)[0] != [2]int{os.Geteuid(), g} {
		t.Fatalf("dono do arquivo novo: chamadas %v, queria uma com %d:%d (o da pasta)", *arquivos, os.Geteuid(), g)
	}
	st, err := os.Lstat(filepath.Join(dir, "relatorio.odt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != 0o600 {
		t.Fatalf("arquivo novo com %v, queria 0600", st.Mode())
	}
}

// Pastas que a restauração cria: cada uma do dono da pasta-mãe. Do root,
// ficavam root 0700, e a Ana não entrava na pasta proj recriada.
func TestPastasCriadasFicamDoDonoDaMae(t *testing.T) {
	arquivos, pastas := registrarDonos(t)
	conteudo := []byte("relatório")
	raiz := t.TempDir()
	g := grupoExtra(t, raiz)
	item := itemDe(filepath.Join(raiz, "proj"), "relatorio.odt", conteudo, "suffix-version")
	item.DestFilename = "sub/relatorio.odt"
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	// O fchown é falso: proj continua com o grupo de quem criou, e sub o herda.
	proj := filepath.Join(raiz, "proj")
	uidProj, gidProj := donoDe(t, proj)
	quer := [][2]int{{os.Geteuid(), g}, {uidProj, gidProj}}
	if len(*pastas) != 2 || (*pastas)[0] != quer[0] || (*pastas)[1] != quer[1] {
		t.Fatalf("donos das pastas criadas: %v, queria %v (o da mãe de cada uma)", *pastas, quer)
	}
	uidSub, gidSub := donoDe(t, filepath.Join(proj, "sub"))
	if len(*arquivos) != 1 || (*arquivos)[0] != [2]int{uidSub, gidSub} {
		t.Fatalf("dono do arquivo: %v, queria o da pasta sub (%d:%d)", *arquivos, uidSub, gidSub)
	}
	for _, d := range []string{proj, filepath.Join(proj, "sub")} {
		st, err := os.Lstat(d)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("%s criada com %v, queria 0700", d, st.Mode().Perm())
		}
	}
}

// Pasta que não é a que o agente criou (aberta para o grupo: outra, posta no
// lugar entre o mkdir e o open) não muda de dono.
func TestPastaAlheiaNaoMudaDeDono(t *testing.T) {
	_, pastas := registrarDonos(t)
	mae := t.TempDir()
	for nome, modo := range map[string]os.FileMode{"do-agente": 0o700, "alheia": 0o750} {
		p := filepath.Join(mae, nome)
		if err := os.Mkdir(p, modo); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, modo); err != nil {
			t.Fatal(err)
		}
	}
	maeFd, err := unix.Open(mae, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(maeFd)
	for _, nome := range []string{"alheia", "do-agente"} {
		fd, err := unix.Openat(maeFd, nome, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := herdarDonoDaMae(maeFd, fd); err != nil {
			t.Fatal(err)
		}
		unix.Close(fd)
	}
	if len(*pastas) != 1 {
		t.Fatalf("fchown chamado %d vezes, queria 1 (só a pasta 0700 do agente): %v", len(*pastas), *pastas)
	}
}

// O mesmo com o chown de verdade: como root, numa pasta de outro dono.
func TestRestauracaoComoRootDaAoDonoDaPasta(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("exige root")
	}
	conteudo := []byte("relatório")
	home := t.TempDir()
	if err := os.Chown(home, 1234, 5678); err != nil {
		t.Fatal(err)
	}
	item := itemDe(filepath.Join(home, "proj"), "relatorio.odt", conteudo, "suffix-version")
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(home, "proj"), filepath.Join(home, "proj", "relatorio.odt")} {
		if uid, gid := donoDe(t, p); uid != 1234 || gid != 5678 {
			t.Fatalf("%s de %d:%d, queria 1234:5678 (o da pasta-mãe)", p, uid, gid)
		}
	}
}
