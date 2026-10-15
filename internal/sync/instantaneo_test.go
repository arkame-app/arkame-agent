package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Com instantâneo, os arquivos são lidos da cópia congelada, e a chave no
// bucket continua a do disco: quem restaura vê a pasta que marcou no plano.
// A pasta do estado local é ignorada também dentro da cópia.
func TestWalkLeDoInstantaneoComAChaveDoDisco(t *testing.T) {
	base := t.TempDir()
	vivo := filepath.Join(base, "vivo")
	congelado := filepath.Join(base, "congelado")
	for _, d := range []string{vivo, filepath.Join(congelado, "estado")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	escrever := func(p, conteudo string) {
		if err := os.WriteFile(p, []byte(conteudo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escrever(filepath.Join(vivo, "a.txt"), "gravando agora")
	escrever(filepath.Join(congelado, "a.txt"), "como estava")
	escrever(filepath.Join(congelado, "so-na-copia.txt"), "x")
	escrever(filepath.Join(congelado, "estado", "estado.db"), "x")

	ler := func(p string) string {
		if resto, ok := strings.CutPrefix(p, vivo); ok {
			return congelado + resto
		}
		return p
	}
	arquivos, erros := walkSem(context.Background(), "/", []string{vivo}, nil, nil,
		[]string{filepath.Join(vivo, "estado")}, nil, ler)
	lidos := map[string]string{}
	for f := range arquivos {
		lidos[f.RelativePath] = f.AbsolutePath
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	chave := strings.TrimPrefix(filepath.ToSlash(vivo), "/")
	queria := map[string]string{
		chave + "/a.txt":           filepath.Join(congelado, "a.txt"),
		chave + "/so-na-copia.txt": filepath.Join(congelado, "so-na-copia.txt"),
	}
	if len(lidos) != len(queria) {
		t.Fatalf("lidos = %v; queria %v", lidos, queria)
	}
	for k, abs := range queria {
		if lidos[k] != abs {
			t.Fatalf("chave %q lida de %q; queria %q (lidos: %v)", k, lidos[k], abs, lidos)
		}
	}
}
