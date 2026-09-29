package sync

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/caminho"
)

// FileInfo é o que o walker emite para cada arquivo encontrado.
type FileInfo struct {
	AbsolutePath string // caminho real na máquina (/host/var/data/file.conf)
	RelativePath string // caminho dentro da source_path (var/data/file.conf)
	Size         int64
	ModTime      int64 // unix nano
}

// Walk percorre os paths fornecidos, respeitando excludeGlobs (patterns tipo *.tmp, node_modules).
// Emite FileInfo via channel. Fecha o channel ao final ou em erro de ctx.
func Walk(ctx context.Context, hostRoot string, sourcePaths []string, excludeGlobs []string) (<-chan FileInfo, <-chan error) {
	out := make(chan FileInfo, 128)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		excludeGlobs := prepararExclusoes(excludeGlobs)
		// Pasta do plano que não abre é falha dela, não do plano: as outras
		// seguem, e o erro vai no fim (o daemon trata como backup parcial).
		var ilegiveis []string
		for _, sp := range sourcePaths {
			root := caminho.NoDisco(hostRoot, sp)

			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					// A própria pasta do plano não abre (não existe, sem
					// permissão, caminho inválido): é falha do backup, e não um
					// backup vazio "concluído" — era o que acontecia com
					// `C:\Users\…` no Windows antes de 28/09.
					if path == root {
						return fmt.Errorf("não consegui ler %s: %w", root, err)
					}
					// arquivo apagado durante walk ou permissão — ignoramos e seguimos
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.IsDir() {
					return nil
				}
				if matchesAny(path, excludeGlobs) {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- FileInfo{
					AbsolutePath: path,
					RelativePath: caminho.NaChave(hostRoot, path),
					Size:         info.Size(),
					ModTime:      info.ModTime().UnixNano(),
				}:
				}
				return nil
			})
			if err != nil && ctx.Err() == nil {
				if strings.HasPrefix(err.Error(), "não consegui ler") {
					ilegiveis = append(ilegiveis, err.Error())
					continue
				}
				errs <- err
				return
			}
		}
		if len(ilegiveis) > 0 {
			errs <- fmt.Errorf("%s", strings.Join(ilegiveis, "; "))
		}
	}()

	return out, errs
}

// prepararExclusoes limpa os padrões uma vez por backup (e não uma vez por
// arquivo): sem vazios e, no Windows, em minúsculas e com `\`.
func prepararExclusoes(globs []string) []string {
	var out []string
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if caminho.Windows {
			g = strings.ToLower(filepath.FromSlash(g))
		}
		out = append(out, g)
	}
	return out
}

func matchesAny(path string, globs []string) bool {
	if len(globs) == 0 {
		return false
	}
	// No Windows os nomes não diferenciam maiúsculas: *.tmp pega ARQUIVO.TMP
	// (os padrões já vêm em minúsculas de prepararExclusoes).
	if caminho.Windows {
		path = strings.ToLower(path)
	}
	base := filepath.Base(path)
	for _, g := range globs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
		// match em qualquer segmento do path (para node_modules etc)
		if strings.Contains(path, string(filepath.Separator)+g+string(filepath.Separator)) ||
			strings.HasSuffix(path, string(filepath.Separator)+g) {
			return true
		}
	}
	return false
}
