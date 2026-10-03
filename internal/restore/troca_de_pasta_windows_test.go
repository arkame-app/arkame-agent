//go:build windows

package restore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Durante o download, uma pasta acima do destino vira junção para outra
// pasta. O CreateTemp já foi feito, mas o rename e a data remontam o caminho
// em texto: sem a conferência do caminho real, o arquivo iria para o alvo da
// junção (C:\Windows\System32, no cenário). Agora para com ErrDestinoLink e
// nada vai para lá. Se o Windows não deixar renomear a pasta (o handle da
// restauração a trava), a restauração tem de dar certo no lugar original.
func TestJuncaoTrocadaDuranteODownloadNaoDesviaAGravacao(t *testing.T) {
	conteudo := []byte("conteúdo da ana")
	base, alheio := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(alheio, "conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	acima := filepath.Join(base, "a")
	destino := filepath.Join(acima, "conf")
	if err := os.MkdirAll(destino, 0o700); err != nil {
		t.Fatal(err)
	}
	trocou := false
	antes := aposBaixar
	t.Cleanup(func() { aposBaixar = antes })
	aposBaixar = func() {
		if err := os.Rename(acima, acima+".old"); err != nil {
			return // travada pelo handle da pasta
		}
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", acima, alheio).CombinedOutput(); err != nil {
			t.Errorf("mklink /J: %v %s", err, out)
			return
		}
		trocou = true
	}

	err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo)},
		itemDe(destino, "app.conf", conteudo, "overwrite"))
	if es, _ := os.ReadDir(filepath.Join(alheio, "conf")); len(es) != 0 {
		t.Fatalf("gravou no alvo da junção: %v", es)
	}
	if trocou {
		if !errors.Is(err, ErrDestinoLink) {
			t.Fatalf("esperava ErrDestinoLink, veio %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(destino, "app.conf")); string(b) != string(conteudo) {
		t.Fatalf("conteúdo restaurado: %q", b)
	}
}
