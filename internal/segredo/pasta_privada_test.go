package segredo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// donoFalso faz as pastas dadas parecerem de outro usuário.
func donoFalso(t *testing.T, alheias ...string) {
	t.Helper()
	antes := donoDeConfianca
	t.Cleanup(func() { donoDeConfianca = antes })
	donoDeConfianca = func(caminho string) (bool, string, error) {
		for _, a := range alheias {
			if filepath.Clean(caminho) == filepath.Clean(a) {
				return false, "uid 4242", nil
			}
		}
		return antes(caminho)
	}
}

// Pasta que não existe: cada nível criado nasce fechado (0700). No Windows é
// a DACL protegida — não dá para conferir aqui.
func TestPastaPrivadaCriaCadaNivelFechado(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "Arkame", "estado")
	if err := PastaPrivada(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Dir(dir)} {
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v, modo %v", p, err, st.Mode().Perm())
		}
	}
	// De novo, já existindo e sendo nossa: nada muda.
	if err := PastaPrivada(dir); err != nil {
		t.Fatal(err)
	}
}

// Pasta que já existia, de dono confiável: serve, e a permissão dela não é
// tocada (pode ser uma pasta do administrador, com outros usos).
func TestPastaPrivadaNaoMexeEmPastaQueJaExistia(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	dir := filepath.Join(t.TempDir(), "do-admin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PastaPrivada(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o755 {
		t.Fatalf("a pasta que já existia mudou de modo: %v", st.Mode().Perm())
	}
}

// Pasta que já existia, criada por outro usuário (o achado A3 da revisão: no
// Windows qualquer usuário cria %ProgramData%\Arkame antes de o agente ser
// instalado): recusada, e nada é criado nem alterado nela.
func TestPastaPrivadaRecusaPastaDeOutroDono(t *testing.T) {
	base := t.TempDir()
	alheia := filepath.Join(base, "Arkame")
	if err := os.Mkdir(alheia, 0o755); err != nil {
		t.Fatal(err)
	}
	donoFalso(t, alheia)

	if err := PastaPrivada(alheia); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta de outro dono: %v", err)
	}
	// Nem dentro dela: quem é dono da de cima troca a nossa por outra.
	dentro := filepath.Join(alheia, "estado")
	if err := PastaPrivada(dentro); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta dentro da de outro dono: %v", err)
	}
	if _, err := os.Stat(dentro); !os.IsNotExist(err) {
		t.Fatal("criou a pasta dentro da de outro dono")
	}
	if err := ConferirDono(alheia); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("ConferirDono: %v", err)
	}
}

// O dono de verdade, neste sistema: o que este usuário cria é dele, e a raiz
// é do root — os dois de confiança. Um arquivo no lugar da pasta não serve.
func TestDonoConfiavelDoSistema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows o dono é um SID; ver proteger_windows.go")
	}
	dir := t.TempDir()
	if err := ConferirDono(dir); err != nil {
		t.Fatalf("a pasta que acabei de criar: %v", err)
	}
	if err := ConferirDono("/"); err != nil {
		t.Fatalf("a raiz (root): %v", err)
	}
	if err := ConferirDono(filepath.Join(dir, "nao-existe")); err == nil {
		t.Fatal("caminho que não existe não tem dono")
	}
	arq := filepath.Join(dir, "arquivo")
	if err := os.WriteFile(arq, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PastaPrivada(arq); err == nil {
		t.Fatal("um arquivo no lugar da pasta não é uma pasta privada")
	}
}

// Com uma pasta de verdade de outro usuário (as de serviços do sistema, onde
// houver): o dono lido do sistema de arquivos é recusado, sem simulação.
func TestDonoDeOutroUsuarioDeVerdade(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa rodar sem root, num sistema com pastas de outros usuários")
	}
	candidatas, _ := filepath.Glob("/var/lib/*")
	for _, p := range candidatas {
		confiavel, dono, err := donoConfiavel(p)
		if err != nil || confiavel {
			continue
		}
		if err := PastaPrivada(p); !errors.Is(err, ErrDonoNaoConfiavel) {
			t.Fatalf("%s (%s): %v", p, dono, err)
		}
		// Dentro dela também não (sem root, a recusa pode vir antes, da
		// própria permissão da pasta).
		if err := PastaPrivada(filepath.Join(p, "estado")); err == nil {
			t.Fatalf("criou uma pasta dentro de %s (%s)", p, dono)
		}
		return
	}
	t.Skip("nenhuma pasta de outro usuário em /var/lib")
}
