package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// No macOS, o serviço sem Acesso Total ao Disco recebe EPERM ("operation not
// permitted") ao ler Mesa, Documentos e Downloads. O Run marca NaoPermitido —
// pela varredura (pasta) e pelo arquivo — para a causa da sessão parcial dizer
// o que fazer. No Linux só se consegue EACCES: o teste troca a checagem.
func TestRunMarcaLeituraNaoPermitida(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lê tudo")
	}
	antes := naoPermitido
	naoPermitido = func(err error) bool { return errors.Is(err, fs.ErrPermission) }
	t.Cleanup(func() { naoPermitido = antes })

	for _, caso := range []string{"pasta", "arquivo"} {
		t.Run(caso, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok"), 0o644); err != nil {
				t.Fatal(err)
			}
			barrado := filepath.Join(dir, "Desktop")
			if caso == "pasta" {
				if err := os.MkdirAll(barrado, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(barrado, "x.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(barrado, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(barrado, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(barrado, 0o755) })

			_, c := novoS3Falso(t)
			r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}})
			if caso == "pasta" && err == nil {
				t.Fatal("pasta barrada deveria deixar o backup parcial")
			}
			if caso == "arquivo" && r.FilesFailed != 1 {
				t.Fatalf("FilesFailed = %d, queria 1 (err=%v)", r.FilesFailed, err)
			}
			if !r.NaoPermitido {
				t.Fatalf("leitura barrada não marcou NaoPermitido (err=%v)", err)
			}
		})
	}

	// Sem nada barrado, não marca.
	naoPermitido = antes
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, c := novoS3Falso(t)
	if r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: "/", SourcePaths: []string{dir}}); err != nil || r.NaoPermitido {
		t.Fatalf("sem leitura barrada: NaoPermitido=%v err=%v", r.NaoPermitido, err)
	}
}
