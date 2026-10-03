package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/caminho"
)

// maxExemplosNaoLidos limita os caminhos citados no erro: o painel mostra a
// mensagem, e mil caminhos não ajudam ninguém.
const maxExemplosNaoLidos = 3

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
		// O que não deu para ler dentro das pastas (subpasta sem permissão,
		// arquivo travado). Antes era ignorado em silêncio: faltava uma
		// subárvore inteira e a sessão saía "concluída". Agora conta, e o
		// backup termina parcial com exemplos do que ficou de fora.
		naoLidos := 0
		var exemplos []string
		anotar := func(p string, err error) {
			naoLidos++
			if len(exemplos) < maxExemplosNaoLidos {
				causa := err
				if u := errors.Unwrap(err); u != nil {
					causa = u // sem repetir o caminho do PathError
				}
				exemplos = append(exemplos, fmt.Sprintf("%s (%v)", p, causa))
			}
		}
		for _, sp := range sourcePaths {
			// O caminho escolhido no plano (como a pessoa o vê) e o caminho de
			// verdade, com os links seguidos no servidor. Lê-se do de verdade;
			// a chave no bucket segue o escolhido: quem marcou /home vê e
			// restaura /home, mesmo que no disco seja /var/home.
			escolhido := caminho.NoDisco(hostRoot, sp)
			root := caminho.Real(hostRoot, sp)
			chave := func(p string) string {
				return caminho.NaChave(hostRoot, escolhido+strings.TrimPrefix(p, root))
			}

			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					// A própria pasta do plano não abre (não existe, sem
					// permissão, caminho inválido): é falha do backup, e não um
					// backup vazio "concluído" — era o que acontecia com
					// `C:\Users\…` no Windows antes de 28/09.
					if path == root {
						return fmt.Errorf("não consegui ler %s: %w", root, err)
					}
					// Apagado durante a leitura: não há o que copiar, e não é
					// falha. O resto (permissão, erro de disco) entra na conta.
					if !errors.Is(err, fs.ErrNotExist) {
						anotar(path, err)
					}
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
				leitura := path
				info, err := d.Info()
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						anotar(path, err)
					}
					return nil
				}
				if d.Type()&fs.ModeSymlink != 0 {
					// Link dentro da pasta: copia o arquivo para onde ele aponta,
					// resolvido no servidor (no Docker, o container resolveria
					// pelo lado dele). Link para pasta não é seguido — evita
					// laço e cópia em dobro do que já está em outro lugar.
					alvo := caminho.Real(hostRoot, strings.TrimPrefix(path, filepath.Clean(hostRoot)))
					st, serr := os.Stat(alvo)
					if serr != nil || !st.Mode().IsRegular() {
						return nil
					}
					leitura, info = alvo, st
				} else if st, ok := irregularLegivel(caminho.Windows, path, info.Mode()); ok {
					info = st
				} else if !info.Mode().IsRegular() {
					// Socket, pipe, dispositivo: não há conteúdo a guardar, e
					// abrir um pipe trava o backup.
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case out <- FileInfo{
					AbsolutePath: leitura,
					RelativePath: chave(path),
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
		if naoLidos > 0 {
			ilegiveis = append(ilegiveis, fmt.Sprintf("%d itens não puderam ser lidos, ex.: %s",
				naoLidos, strings.Join(exemplos, "; ")))
		}
		if len(ilegiveis) > 0 {
			errs <- fmt.Errorf("%s", strings.Join(ilegiveis, "; "))
		}
	}()

	return out, errs
}

// irregularLegivel diz se uma entrada que o Go marca como irregular é, no
// Windows, um arquivo comum para o backup — e devolve a ficha dele.
//
// Desde o Go 1.23, no Windows, todo reparse point que não é link simbólico (nem
// soquete, nem arquivo deduplicado) sai com fs.ModeIrregular. É o caso dos
// arquivos do OneDrive (IO_REPARSE_TAG_CLOUD_*, inclusive os "sempre manter
// neste dispositivo") e de outros filtros de arquivo. O walker pulava tudo o que
// não era regular, calado: com a Área de Trabalho e os Documentos redirecionados
// para o OneDrive — o padrão do Windows 11 —, o backup dessas pastas saía vazio
// e "concluído".
//
// Escolha: tratar aqui, só no Windows, em vez de `//go:debug winsymlink=0` no
// main. O godebug volta o programa inteiro ao comportamento de antes do 1.23
// (junções viram links simbólicos também no navegador de pastas e na
// restauração), e o Go avisa que essas chaves de compatibilidade saem um dia.
// Aqui a regra fica explícita e testada: segue-se o reparse point (os.Stat) e
// copia-se o que for arquivo; o que der em pasta (junção, ponto de montagem)
// não é seguido — como os links de pasta —, e o que não abre fica de fora.
// Ler um arquivo do OneDrive que só está na nuvem baixa o conteúdo: é o que o
// backup precisa.
func irregularLegivel(windows bool, path string, modo fs.FileMode) (fs.FileInfo, bool) {
	if !windows || modo&fs.ModeIrregular == 0 || modo.IsDir() {
		return nil, false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil, false
	}
	// O próprio Stat do Go devolve de novo ModeIrregular para esses reparse
	// points (ele não os segue); o que importa é não ser pasta, pipe nem
	// dispositivo.
	if st.Mode().Type()&^fs.ModeIrregular != 0 {
		return nil, false
	}
	return st, true
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
