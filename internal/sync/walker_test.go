package sync

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A pasta do plano que não abre é falha do backup, e não um backup vazio
// "concluído" — era o que acontecia com `C:\Users\…` no Windows (28/09).
func TestWalkPastaInexistenteFalha(t *testing.T) {
	arquivos, erros := Walk(context.Background(), "/", []string{filepath.Join(t.TempDir(), "nao-existe")}, nil)
	for range arquivos {
	}
	err := <-erros
	if err == nil || !strings.Contains(err.Error(), "não consegui ler") {
		t.Fatalf("esperava falha ao ler a pasta do plano, veio %v", err)
	}
}

// Uma pasta que não abre não impede as outras do plano.
func TestWalkUmaPastaRuimNaoBloqueiaAsOutras(t *testing.T) {
	boa := t.TempDir()
	if err := os.WriteFile(filepath.Join(boa, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), "/", []string{filepath.Join(boa, "nao-existe"), boa}, nil)
	n := 0
	for range arquivos {
		n++
	}
	if err := <-erros; err == nil || n != 1 {
		t.Fatalf("esperava 1 arquivo da pasta boa e o erro da ruim; veio %d arquivos, erro %v", n, err)
	}
}

func TestWalkChaveRelativaAoHostRoot(t *testing.T) {
	raiz := t.TempDir()
	if err := os.MkdirAll(filepath.Join(raiz, "var", "dados"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "var", "dados", "A.TMP"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "var", "dados", "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), raiz, []string{"/var/dados"}, []string{"*.tmp"})
	var chaves []string
	for f := range arquivos {
		chaves = append(chaves, f.RelativePath)
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	// No Linux, *.tmp não pega A.TMP (nomes diferenciam maiúsculas); no Windows pega.
	if len(chaves) != 2 || chaves[0] != "var/dados/A.TMP" && chaves[1] != "var/dados/A.TMP" {
		t.Fatalf("chaves = %v", chaves)
	}
}

// Origem que é link (/home no Fedora Atomic): lê o destino, mas a chave no
// bucket segue o caminho escolhido no plano. Link de arquivo dentro da pasta
// copia o arquivo para onde aponta, resolvido no servidor.
func TestWalkOrigemQueELink(t *testing.T) {
	raiz := t.TempDir()
	casa := filepath.Join(raiz, "var", "home", "hugo")
	if err := os.MkdirAll(casa, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(casa, "nota.txt"), []byte("oi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(raiz, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "etc", "conf"), []byte("servidor"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("var/home", filepath.Join(raiz, "home")); err != nil {
		t.Fatal(err)
	}
	// Link absoluto para /etc/conf: no Docker, tem de ser o /etc do servidor.
	if err := os.Symlink("/etc/conf", filepath.Join(casa, "conf-link")); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), raiz, []string{"/home/hugo"}, nil)
	got := map[string]string{}
	for f := range arquivos {
		got[f.RelativePath] = f.AbsolutePath
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	if got["home/hugo/nota.txt"] != filepath.Join(casa, "nota.txt") {
		t.Fatalf("arquivo pela origem-link: %v", got)
	}
	if got["home/hugo/conf-link"] != filepath.Join(raiz, "etc", "conf") {
		t.Fatalf("link absoluto deveria apontar para o /etc do servidor: %v", got)
	}
}

// lerReparseFalso troca a leitura do disco por atributos e tag fixos.
func lerReparseFalso(t *testing.T, atributos, tag uint32) {
	t.Helper()
	antes := lerReparse
	lerReparse = func(string) (uint32, uint32, error) { return atributos, tag, nil }
	t.Cleanup(func() { lerReparse = antes })
}

// No Windows (Go ≥ 1.23), arquivo do OneDrive é ModeIrregular: tem de entrar no
// backup como arquivo comum. Pasta (junção) não é seguida, e fora do Windows a
// regra não vale.
func TestIrregularLegivelNoWindows(t *testing.T) {
	dir := t.TempDir()
	arq := filepath.Join(dir, "nuvem.docx")
	if err := os.WriteFile(arq, []byte("conteúdo do OneDrive"), 0o644); err != nil {
		t.Fatal(err)
	}
	lerReparseFalso(t, 0x20|0x400, 0x9000601A) // ARCHIVE|REPARSE_POINT, CLOUD_6
	st, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666)
	if classe != reparseCopiar || st.Size() != int64(len("conteúdo do OneDrive")) {
		t.Fatalf("arquivo do OneDrive no disco deveria entrar: classe=%v st=%v", classe, st)
	}
	if _, classe := irregularLegivel(true, dir, fs.ModeIrregular|0o666); classe == reparseCopiar {
		t.Fatal("reparse point que dá em pasta (junção) não deveria ser copiado como arquivo")
	}
	if _, classe := irregularLegivel(true, filepath.Join(dir, "sumiu"), fs.ModeIrregular); classe == reparseCopiar {
		t.Fatal("o que não abre fica de fora")
	}
	if _, classe := irregularLegivel(false, arq, fs.ModeIrregular|0o666); classe != reparseIgnorar {
		t.Fatal("fora do Windows, irregular continua de fora")
	}
	if _, classe := irregularLegivel(true, arq, 0o666); classe != reparseIgnorar {
		t.Fatal("arquivo regular segue o caminho comum")
	}
}

// O AppExecLink (%LOCALAPPDATA%\Microsoft\WindowsApps\*.exe) passa no
// os.Stat mas não abre: entrava no backup, falhava, e todo plano com o perfil
// do usuário saía parcial, todo dia. Não é arquivo de nuvem: fica de fora.
func TestIrregularLegivelAppExecLinkFicaDeFora(t *testing.T) {
	arq := filepath.Join(t.TempDir(), "winget.exe")
	if err := os.WriteFile(arq, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lerReparseFalso(t, 0x20|0x400, 0x8000001B) // IO_REPARSE_TAG_APPEXECLINK
	if st, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparseIgnorar {
		t.Fatalf("AppExecLink não é arquivo de nuvem: classe=%v st=%v", classe, st)
	}
}

// Arquivo do OneDrive só na nuvem: não é copiado (baixaria o arquivo) nem é
// falha — o walker conta à parte.
func TestIrregularLegivelSoNaNuvem(t *testing.T) {
	arq := filepath.Join(t.TempDir(), "grande.mp4")
	if err := os.WriteFile(arq, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lerReparseFalso(t, 0x400|0x00400000|0x1000, 0x9000001A)
	if _, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparseSoNaNuvem {
		t.Fatalf("arquivo só na nuvem deveria ser contado à parte, veio %v", classe)
	}
}

// Tag que não deu para ler: conta como pulado (não se sabe se é dado do
// usuário). Apagado no meio da leitura: não há o que copiar nem contar.
func TestIrregularLegivelTagIlegivel(t *testing.T) {
	arq := filepath.Join(t.TempDir(), "x.dat")
	if err := os.WriteFile(arq, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	antes := lerReparse
	t.Cleanup(func() { lerReparse = antes })
	lerReparse = func(string) (uint32, uint32, error) { return 0, 0, fs.ErrPermission }
	if _, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparsePulado {
		t.Fatalf("tag ilegível deveria contar como pulado, veio %v", classe)
	}
	lerReparse = func(string) (uint32, uint32, error) { return 0, 0, fs.ErrNotExist }
	if _, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparseIgnorar {
		t.Fatalf("apagado durante a leitura não conta, veio %v", classe)
	}
}

// Arquivo compactado pelo WOF (o Go o entrega como irregular) e arquivo
// otimizado pela Desduplicação de Dados: a leitura comum devolve o conteúdo,
// e eles entram no backup. Só se não abrirem contam como pulados.
func TestIrregularLegivelWofEDedupEntram(t *testing.T) {
	arq := filepath.Join(t.TempDir(), "compactado.dll")
	if err := os.WriteFile(arq, []byte("conteúdo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []uint32{0x80000017, 0x80000013} {
		lerReparseFalso(t, 0x20|0x400, tag)
		st, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666)
		if classe != reparseCopiar || st == nil || st.Size() != int64(len("conteúdo")) {
			t.Fatalf("tag %#x deveria entrar como arquivo: classe=%v st=%v", tag, classe, st)
		}
	}
	antes := abrirParaLer
	t.Cleanup(func() { abrirParaLer = antes })
	abrirParaLer = func(string) (*os.File, error) { return nil, fs.ErrPermission }
	if _, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparsePulado {
		t.Fatalf("WOF/dedup que não abre deveria contar como pulado, veio %v", classe)
	}
	abrirParaLer = func(string) (*os.File, error) { return nil, fs.ErrNotExist }
	if _, classe := irregularLegivel(true, arq, fs.ModeIrregular|0o666); classe != reparseIgnorar {
		t.Fatalf("WOF/dedup apagado no meio da leitura não conta, veio %v", classe)
	}
}

func TestClassificarReparse(t *testing.T) {
	const reparse = 0x400
	casos := []struct {
		nome      string
		atributos uint32
		tag       uint32
		quer      classeReparse
	}{
		{"CLOUD no disco", reparse, 0x9000001A, reparseCopiar},
		{"CLOUD_1 no disco", reparse | 0x20, 0x9000101A, reparseCopiar},
		{"CLOUD_F no disco (fixado)", reparse | 0x00080000, 0x9000F01A, reparseCopiar},
		{"CLOUD só na nuvem (recall on data access)", reparse | 0x00400000, 0x9000301A, reparseSoNaNuvem},
		{"CLOUD só na nuvem (recall on open)", reparse | 0x00040000, 0x9000001A, reparseSoNaNuvem},
		{"CLOUD offline", reparse | 0x1000, 0x9000201A, reparseSoNaNuvem},
		{"CLOUD que é pasta", reparse | 0x10, 0x9000001A, reparseIgnorar},
		{"AppExecLink", reparse, 0x8000001B, reparseIgnorar},
		{"symlink", reparse, 0xA000000C, reparseIgnorar},
		{"junção", reparse, 0xA0000003, reparseIgnorar},
		{"dedup", reparse, 0x80000013, reparseLegivel},
		{"WOF (compactado)", reparse, 0x80000017, reparseLegivel},
		{"WOF que é pasta", reparse | 0x10, 0x80000017, reparseIgnorar},
		{"Azure File Sync em camada fria", reparse | 0x1000, 0x8000001E, reparsePulado},
		{"HSM", reparse, 0xC0000004, reparsePulado},
		{"parecido com CLOUD em outro nibble", reparse, 0x9001001A, reparsePulado},
		{"sem tag", 0x20, 0, reparsePulado},
	}
	for _, c := range casos {
		if got := classificarReparse(c.atributos, c.tag); got != c.quer {
			t.Errorf("%s (attr=%#x tag=%#x): veio %v, queria %v", c.nome, c.atributos, c.tag, got, c.quer)
		}
	}
}

// Subpasta sem permissão dentro do plano: os outros arquivos seguem, mas o
// walker avisa no fim (o daemon marca a sessão parcial). Antes, a subárvore
// sumia do backup e a sessão saía "concluída".
func TestWalkSubpastaIlegivelViraErro(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lê pasta sem permissão")
	}
	raiz := t.TempDir()
	if err := os.WriteFile(filepath.Join(raiz, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	trancada := filepath.Join(raiz, "trancada")
	if err := os.MkdirAll(trancada, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trancada, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })

	arquivos, erros := Walk(context.Background(), "/", []string{raiz}, nil)
	n := 0
	for range arquivos {
		n++
	}
	err := <-erros
	if n != 1 {
		t.Fatalf("esperava o arquivo legível, veio %d", n)
	}
	if err == nil || !strings.Contains(err.Error(), "1 itens não puderam ser lidos") || !strings.Contains(err.Error(), "trancada") {
		t.Fatalf("a subpasta ilegível deveria virar erro (backup parcial), veio %v", err)
	}
}

// Pasta excluída do plano (node_modules) não é percorrida: uma subpasta
// ilegível lá dentro não pode deixar o backup parcial. Antes o walker entrava
// em toda pasta e só aplicava a exclusão aos arquivos.
func TestWalkPastaExcluidaNaoEPercorrida(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de permissão de pasta POSIX e de não ser root")
	}
	raiz := t.TempDir()
	if err := os.WriteFile(filepath.Join(raiz, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	modulos := filepath.Join(raiz, "app", "node_modules")
	trancada := filepath.Join(modulos, "pacote", "trancada")
	if err := os.MkdirAll(trancada, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modulos, "pacote", "index.js"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })
	cache := filepath.Join(raiz, ".cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "c.bin"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}

	arquivos, erros := Walk(context.Background(), "/", []string{raiz}, []string{"node_modules", ".cache"})
	var vistos []string
	for f := range arquivos {
		vistos = append(vistos, f.AbsolutePath)
	}
	if err := <-erros; err != nil {
		t.Fatalf("erro dentro de pasta excluída não deveria deixar o backup parcial: %v", err)
	}
	if len(vistos) != 1 || filepath.Base(vistos[0]) != "a.txt" {
		t.Fatalf("esperava só a.txt, veio %v", vistos)
	}
}

// Link para arquivo cujo destino não dá para ler entra na conta do backup
// parcial, como um arquivo comum ilegível. Antes era pulado calado: o link
// sumia de uma sessão "concluída" e o painel lia a falta como remoção. Link
// quebrado e link para pasta continuam fora, sem deixar a sessão parcial.
func TestWalkLinkComDestinoIlegivelViraErro(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de permissão de pasta POSIX e de não ser root")
	}
	raiz := t.TempDir()
	if err := os.WriteFile(filepath.Join(raiz, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fora := t.TempDir()
	trancada := filepath.Join(fora, "trancada")
	if err := os.MkdirAll(trancada, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trancada, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })
	links := map[string]string{
		"barrado":  filepath.Join(trancada, "b.txt"),
		"quebrado": filepath.Join(fora, "nao-existe"),
		"pasta":    fora,
	}
	for nome, alvo := range links {
		if err := os.Symlink(alvo, filepath.Join(raiz, nome)); err != nil {
			t.Fatal(err)
		}
	}

	arquivos, erros := Walk(context.Background(), "/", []string{raiz}, nil)
	var vistos []string
	for f := range arquivos {
		vistos = append(vistos, filepath.Base(f.AbsolutePath))
	}
	err := <-erros
	if len(vistos) != 1 || vistos[0] != "a.txt" {
		t.Fatalf("esperava só a.txt, veio %v", vistos)
	}
	if err == nil || !strings.Contains(err.Error(), "1 itens não puderam ser lidos") ||
		!strings.Contains(err.Error(), "barrado") {
		t.Fatalf("o link com destino ilegível deveria virar erro (backup parcial), veio %v", err)
	}
	if strings.Contains(err.Error(), "quebrado") || strings.Contains(err.Error(), "pasta") {
		t.Fatalf("link quebrado ou para pasta não é falha: %v", err)
	}
}
