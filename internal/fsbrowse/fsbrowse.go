// Package fsbrowse lista diretórios do host para o explorador de pastas do
// wizard de plano no painel (pull model: o painel enfileira a requisição e o
// daemon responde). Somente metadados — nenhum conteúdo de arquivo é lido.
package fsbrowse

import (
	"fmt"
	"os"
	"sort"

	"github.com/arkame-app/agent/internal/caminho"
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

// ListDir lista um diretório do servidor, pelo caminho do plano. No Linux,
// dentro do HostRoot (no Docker, /host — sem isso o navegador listava o
// container). No Windows, a raiz é a lista de unidades (C:, D:…), e os caminhos
// vêm no formato do Windows (fundador, 28/09: "path deve ser absoluto: /").
func ListDir(hostRoot, path string) ([]Entry, error) {
	if caminho.Windows && caminho.Raiz(path) {
		return unidades(), nil
	}
	if !caminho.Absoluto(path) {
		return nil, fmt.Errorf("path deve ser absoluto: %q", path)
	}
	clean := caminho.NoDisco(hostRoot, path)

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

// unidades lista as unidades do Windows que existem (C:, D:…), como pastas.
func unidades() []Entry {
	var e []Entry
	for l := 'A'; l <= 'Z'; l++ {
		if st, err := os.Stat(string(l) + `:\`); err == nil && st.IsDir() {
			e = append(e, Entry{Name: string(l) + ":", Dir: true})
		}
	}
	return e
}
