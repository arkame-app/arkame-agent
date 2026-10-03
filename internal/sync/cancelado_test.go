package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Serviço parando durante a varredura: o walker sai sem erro (o cancelamento
// não é falha dele), e o Run devolvia nil — a sessão saía "complete" com só
// parte dos arquivos, e o painel lia a falta do resto como remoção.
func TestRunCanceladoDevolveErro(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, c := novoS3Falso(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancelado devolveu %v, queria context.Canceled", err)
	}
}
