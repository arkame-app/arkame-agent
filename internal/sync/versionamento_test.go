package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Bucket sem versionamento: a primeira resposta sem VersionId encerra o Run
// com ErrBucketSemVersionamento, sem subir o resto à toa.
func TestRunBucketSemVersionamento(t *testing.T) {
	falso, c := novoS3Falso(t)
	falso.semVersao = true
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}})
	if !errors.Is(err, ErrBucketSemVersionamento) {
		t.Fatalf("esperava ErrBucketSemVersionamento, veio %v", err)
	}
	if len(r.VersionMap) != 0 {
		t.Fatalf("entrada sem VersionId no version_map: %+v", r.VersionMap)
	}
	if n := len(falso.objetos); n != 1 {
		t.Fatalf("seguiu subindo depois de saber que o bucket não versiona: %d objetos", n)
	}
}

// Dedup de um objeto que subiu sem versão também não vale.
func TestProcessFileDedupSemVersao(t *testing.T) {
	falso, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "igual")
	o := EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}
	falso.objetos["data/a/dados.db"] = objetoFalso{dados: []byte("igual"), sha256: sha256Hex([]byte("igual")), versao: "v9"}
	falso.semVersao = true
	_, _, err := processFile(context.Background(), o, fi)
	if !errors.Is(err, ErrBucketSemVersionamento) {
		t.Fatalf("esperava ErrBucketSemVersionamento, veio %v", err)
	}
}
