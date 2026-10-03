package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// Objeto reaproveitado (mesmo hash): o índice leva a data do arquivo, não o
// LastModified do objeto no bucket.
func TestDedupIndexaADataDoArquivo(t *testing.T) {
	falso, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "igual")
	quando := time.Date(2023, 7, 1, 12, 0, 0, 0, time.UTC)
	fi.ModTime = quando
	falso.objetos["data/a/dados.db"] = objetoFalso{dados: []byte("igual"), sha256: sha256Hex([]byte("igual")), versao: "v9"}
	falso.ultimaModificacao = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	e, dedup, err := processFile(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}, fi)
	if err != nil || !dedup {
		t.Fatalf("err=%v dedup=%v", err, dedup)
	}
	if !e.ModifiedAt.Equal(quando) {
		t.Fatalf("modified_at = %v, queria a data do arquivo %v", e.ModifiedAt, quando)
	}
	if e.VersionID != "v9" {
		t.Fatalf("version_id = %q", e.VersionID)
	}
}
