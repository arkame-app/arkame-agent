package sync

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
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

		for _, sp := range sourcePaths {
			root := filepath.Join(hostRoot, sp)

			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
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
				rel, err := filepath.Rel(hostRoot, path)
				if err != nil {
					rel = strings.TrimPrefix(path, hostRoot)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- FileInfo{
					AbsolutePath: path,
					RelativePath: filepath.ToSlash(rel),
					Size:         info.Size(),
					ModTime:      info.ModTime().UnixNano(),
				}:
				}
				return nil
			})
			if err != nil && ctx.Err() == nil {
				errs <- err
				return
			}
		}
	}()

	return out, errs
}

func matchesAny(path string, globs []string) bool {
	if len(globs) == 0 {
		return false
	}
	base := filepath.Base(path)
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
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
