package sync

import (
	"context"
	"errors"
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

// Reparse point de outro filtro (Azure File Sync em camada fria, HSM) ou cuja
// tag não deu para ler fica de fora, mas conta: antes saía calado, a sessão
// terminava "completa" e o painel lia a falta como remoção na origem.
func TestRunContaOsReparsePointsPulados(t *testing.T) {
	antesLer := lerReparse
	lerReparse = func(p string) (uint32, uint32, error) {
		switch {
		case strings.HasSuffix(p, ".afs"):
			return 0x400 | 0x1000, 0x8000001E, nil // IO_REPARSE_TAG_STORAGE_SYNC
		case strings.HasSuffix(p, ".hsm"):
			return 0x400, 0xC0000004, nil // IO_REPARSE_TAG_HSM
		case strings.HasSuffix(p, ".wof"):
			return 0x400, 0x80000017, nil // IO_REPARSE_TAG_WOF: entra
		case strings.HasSuffix(p, ".negado"):
			return 0, 0, fs.ErrPermission
		}
		return 0, 0, errors.New("não é reparse point")
	}
	t.Cleanup(func() { lerReparse = antesLer })
	antes := classificarEntrada
	classificarEntrada = func(_ bool, path string, modo fs.FileMode) (fs.FileInfo, classeReparse) {
		if strings.HasSuffix(path, ".txt") {
			return antes(true, path, modo)
		}
		return irregularLegivel(true, path, modo|fs.ModeIrregular)
	}
	t.Cleanup(func() { classificarEntrada = antes })

	dir := t.TempDir()
	for _, n := range []string{"local.txt", "a.afs", "b.hsm", "c.negado", "d.wof"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, c := novoS3Falso(t)
	r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}})
	if err != nil {
		t.Fatalf("reparse point de outro filtro não é falha: %v", err)
	}
	if len(r.VersionMap) != 2 || !strings.HasSuffix(r.VersionMap[0].Key, "d.wof") || !strings.HasSuffix(r.VersionMap[1].Key, "local.txt") {
		t.Fatalf("version_map = %+v, queria d.wof (WOF entra) e local.txt", r.VersionMap)
	}
	if r.ReparseSkipped != 3 {
		t.Fatalf("ReparseSkipped = %d, queria 3 (Azure File Sync, HSM, ilegível)", r.ReparseSkipped)
	}
	if r.CloudOnlySkipped != 0 {
		t.Fatalf("CloudOnlySkipped = %d, queria 0", r.CloudOnlySkipped)
	}
}
