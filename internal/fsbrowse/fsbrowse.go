// Package fsbrowse lista diretórios do host para o explorador de pastas do
// wizard de plano no painel (pull model: o painel enfileira a requisição e o
// daemon responde). Somente metadados — nenhum conteúdo de arquivo é lido.
package fsbrowse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Entry é um item de diretório reportado ao painel.
type Entry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

// MaxEntries limita a resposta — diretórios gigantes são truncados (o painel
// só precisa da estrutura para seleção de pastas).
const MaxEntries = 1000

// ListDir lista um diretório absoluto do host. Retorna erro para paths
// relativos ou não-diretórios; symlinks aparecem como arquivos (não segue).
func ListDir(path string) ([]Entry, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path deve ser absoluto: %q", path)
	}
	clean := filepath.Clean(path)

	dirents, err := os.ReadDir(clean)
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(dirents))
	for _, d := range dirents {
		e := Entry{Name: d.Name(), Dir: d.IsDir()}
		if !e.Dir {
			if info, ierr := d.Info(); ierr == nil {
				e.Size = info.Size()
			}
		}
		entries = append(entries, e)
	}

	// Pastas primeiro, depois alfabético — espelha a ordenação do painel.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Name < entries[j].Name
	})

	if len(entries) > MaxEntries {
		entries = entries[:MaxEntries]
	}
	return entries, nil
}
