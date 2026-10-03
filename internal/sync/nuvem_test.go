package sync

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Arquivo do OneDrive só na nuvem fica de fora (lê-lo baixaria o arquivo),
// mas a sessão não pode sair como inventário completo: o Run devolve quantos
// ficaram de fora, para o /complete levar ao painel. Antes a contagem ia só
// para o log, e o painel lia a falta deles como "removido na origem".
func TestRunContaOsArquivosSoNaNuvem(t *testing.T) {
	antes := classificarEntrada
	classificarEntrada = func(windows bool, path string, modo fs.FileMode) (fs.FileInfo, classeReparse) {
		if strings.HasSuffix(path, ".nuvem") {
			return nil, reparseSoNaNuvem
		}
		return antes(windows, path, modo)
	}
	t.Cleanup(func() { classificarEntrada = antes })

	dir := t.TempDir()
	for _, n := range []string{"local.txt", "a.nuvem", "b.nuvem"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, c := novoS3Falso(t)
	r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}})
	if err != nil {
		t.Fatalf("arquivo só na nuvem não é falha: %v", err)
	}
	if len(r.VersionMap) != 1 || !strings.HasSuffix(r.VersionMap[0].Key, "local.txt") {
		t.Fatalf("version_map = %+v, queria só local.txt", r.VersionMap)
	}
	if r.CloudOnlySkipped != 2 {
		t.Fatalf("CloudOnlySkipped = %d, queria 2", r.CloudOnlySkipped)
	}
}
