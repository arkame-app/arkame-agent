package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A exclusão olha só o caminho dentro da pasta do plano. Com o caminho
// absoluto, um segmento da própria pasta escolhida (AppData, cache, o host do
// /host no Docker) excluía tudo.
func TestExcluidoSoPeloCaminhoRelativo(t *testing.T) {
	casos := []struct {
		nome    string
		windows bool
		raiz, p string
		globs   []string
		quer    bool
	}{
		// Linux
		{"segmento da pasta do plano não conta", false, "/var/cache/app", "/var/cache/app/dados.db", []string{"cache"}, false},
		{"host do Docker não conta", false, "/host/srv", "/host/srv/a.txt", []string{"host"}, false},
		{"pasta do plano em si nunca", false, "/srv/node_modules", "/srv/node_modules", []string{"node_modules"}, false},
		{"segmento dentro da pasta conta", false, "/var/cache/app", "/var/cache/app/x/cache/y.bin", []string{"cache"}, true},
		{"pasta excluída dentro", false, "/srv", "/srv/app/node_modules", []string{"node_modules"}, true},
		{"nome por glob", false, "/srv", "/srv/a/b.tmp", []string{"*.tmp"}, true},
		{"glob não casa a pasta do plano", false, "/srv/x.tmp", "/srv/x.tmp/a.txt", []string{"*.tmp"}, false},
		{"vários segmentos", false, "/srv", "/srv/app/cache/a", []string{"app/cache"}, true},
		{"raiz do disco", false, "/", "/etc/cache/a", []string{"cache"}, true},
		{"sem padrão", false, "/srv", "/srv/a", nil, false},
		// Windows (padrões já em minúsculas e com \, como prepararExclusoes deixa)
		{"AppData da pasta do plano não conta", true, `C:\Users\Ana\AppData\Roaming\Thunderbird`, `C:\Users\Ana\AppData\Roaming\Thunderbird\perfil\inbox`, []string{"appdata"}, false},
		{"AppData dentro conta", true, `C:\Users\Ana`, `C:\Users\Ana\AppData\Local\x.db`, []string{"appdata"}, true},
		{"maiúsculas no Windows", true, `C:\Dados`, `C:\Dados\Sub\ARQUIVO.TMP`, []string{"*.tmp"}, true},
		{"vários segmentos no Windows", true, `C:\Users\Ana`, `C:\Users\Ana\AppData\Local\Temp\a`, []string{`appdata\local\temp`}, true},
		{"vários segmentos fora da pasta não contam", true, `C:\Users\Ana\AppData\Local\Temp`, `C:\Users\Ana\AppData\Local\Temp\a`, []string{`appdata\local\temp`}, false},
		{"raiz da unidade", true, `C:\`, `C:\Users\Ana\AppData`, []string{"appdata"}, true},
		{"pasta do plano no Windows", true, `C:\Users\Ana\AppData`, `C:\Users\Ana\AppData`, []string{"appdata"}, false},
	}
	for _, c := range casos {
		if got := excluido(c.windows, c.raiz, c.p, c.globs); got != c.quer {
			t.Errorf("%s: excluido(%q, %q, %v) = %v, queria %v", c.nome, c.raiz, c.p, c.globs, got, c.quer)
		}
	}
}

// No walker: a pasta do plano com "cache" no caminho é copiada; um "cache"
// dentro dela, não.
func TestWalkExclusaoNaoOlhaACaminhoDaPasta(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("caminho POSIX")
	}
	raiz := filepath.Join(t.TempDir(), "cache", "app")
	if err := os.MkdirAll(filepath.Join(raiz, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "dados.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "cache", "lixo"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), "/", []string{raiz}, []string{"cache"})
	var vistos []string
	for f := range arquivos {
		vistos = append(vistos, filepath.Base(f.AbsolutePath))
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	if len(vistos) != 1 || vistos[0] != "dados.db" {
		t.Fatalf("esperava só dados.db, veio %v", vistos)
	}
}
