package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	// ModTime é a data do arquivo como o sistema a dá. Era int64 em
	// nanossegundos (UnixNano), que só cobre 1677–2262: um arquivo com data
	// fora disso (relógio errado, arquivo de teste) ia ao índice com a data
	// dando a volta.
	ModTime time.Time
}

// Walk percorre os paths fornecidos, respeitando excludeGlobs (patterns tipo *.tmp, node_modules).
// Emite FileInfo via channel. Fecha o channel ao final ou em erro de ctx.
func Walk(ctx context.Context, hostRoot string, sourcePaths []string, excludeGlobs []string) (<-chan FileInfo, <-chan error) {
	return walk(ctx, hostRoot, sourcePaths, excludeGlobs, nil)
}

// puladosDoWalk é o que ficou de fora da varredura sem deixar o backup
// parcial, mas que o painel precisa saber para não ler a falta como remoção.
type puladosDoWalk struct {
	SoNaNuvem int // arquivos do OneDrive só na nuvem
	Reparse   int // outros reparse points (Azure File Sync, HSM) ou ilegíveis
	// NaoPermitido: alguma leitura voltou EPERM ("operation not permitted"),
	// que no macOS é o TCC barrando o serviço sem Acesso Total ao Disco.
	NaoPermitido bool
}

// naoPermitido diz se o erro é EPERM. Variável para os testes, que no Linux
// só conseguem um EACCES.
var naoPermitido = func(err error) bool { return errors.Is(err, syscall.EPERM) }

// walk é o Walk que, com puladosOut, devolve quantos arquivos ficaram de fora
// por estarem só na nuvem ou por serem reparse points de outro filtro. O valor
// é gravado antes de o canal de erros fechar: só se lê depois de vê-lo fechado
// (receber o erro não basta, ele vai antes da gravação).
func walk(ctx context.Context, hostRoot string, sourcePaths []string, excludeGlobs []string, puladosOut *puladosDoWalk) (<-chan FileInfo, <-chan error) {
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
		// Arquivos do OneDrive que só estão na nuvem: ficam de fora sem
		// deixar o backup parcial. Vão ao log, num resumo, e a contagem vai
		// ao painel (cloud_only_skipped): sem ela, a falta deles na sessão
		// "completa" parecia arquivo removido na origem.
		soNaNuvem := 0
		exemploNuvem := ""
		// Reparse points que não são do OneDrive (Azure File Sync em camada
		// fria, HSM) ou que não deu para ler: mesma regra, contados à parte
		// (reparse_skipped). Antes ficavam de fora calados.
		reparse := 0
		exemploReparse := ""
		eperm := false
		if puladosOut != nil {
			defer func() {
				*puladosOut = puladosDoWalk{SoNaNuvem: soNaNuvem, Reparse: reparse, NaoPermitido: eperm}
			}()
		}
		anotar := func(p string, err error) {
			naoLidos++
			eperm = eperm || naoPermitido(err)
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
						eperm = eperm || naoPermitido(err)
						return fmt.Errorf("não consegui ler %s: %w", root, err)
					}
					// O que está excluído do plano não entra na conta: não ia
					// para o backup de qualquer jeito.
					if excluido(caminho.Windows, root, path, excludeGlobs) {
						if d != nil && d.IsDir() {
							return filepath.SkipDir
						}
						return nil
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
				// Pasta excluída (node_modules, .cache) nem é aberta: entrar
				// nela lia milhares de arquivos à toa, e um erro de leitura lá
				// dentro deixava todo backup "parcial". Mesma regra dos
				// arquivos (nome ou segmento do caminho dentro da pasta do
				// plano); a pasta do plano em si nunca é pulada.
				if excluido(caminho.Windows, root, path, excludeGlobs) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
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
					if serr != nil {
						// Link quebrado não tem o que copiar. Destino que não
						// deu para ler (EACCES, ou o macOS sem Acesso Total ao
						// Disco) entra na conta, como um arquivo comum: calado,
						// o link sumia de uma sessão "concluída" e o painel lia
						// a falta como remoção.
						if !errors.Is(serr, fs.ErrNotExist) {
							anotar(path, serr)
						}
						return nil
					}
					if !st.Mode().IsRegular() {
						return nil
					}
					leitura, info = alvo, st
				} else if st, classe := classificarEntrada(caminho.Windows, path, info.Mode()); classe == reparseCopiar {
					info = st
				} else if classe == reparseSoNaNuvem {
					// Só na nuvem: não é falha, e lê-lo baixaria o arquivo.
					soNaNuvem++
					if exemploNuvem == "" {
						exemploNuvem = path
					}
					return nil
				} else if classe == reparsePulado {
					reparse++
					if exemploReparse == "" {
						exemploReparse = path
					}
					return nil
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
					ModTime:      info.ModTime(),
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
		if soNaNuvem > 0 {
			slog.Info("arquivos só na nuvem (OneDrive) ficaram de fora do backup",
				"quantidade", soNaNuvem, "exemplo", exemploNuvem)
		}
		if reparse > 0 {
			slog.Warn("reparse points fora do OneDrive (Azure File Sync, HSM) ou ilegíveis ficaram de fora do backup",
				"quantidade", reparse, "exemplo", exemploReparse)
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

// abrirParaLer é o os.Open da checagem do WOF/dedup; os testes o trocam.
var abrirParaLer = os.Open

// classificarEntrada é irregularLegivel, numa variável para os testes do
// walker simularem, fora do Windows, um arquivo que só está na nuvem.
var classificarEntrada = irregularLegivel

// irregularLegivel diz se uma entrada que o Go marca como irregular é, no
// Windows, um arquivo de nuvem para o backup — e devolve a ficha dele.
//
// Desde o Go 1.23, no Windows, todo reparse point que não é link simbólico (nem
// soquete, nem arquivo deduplicado) sai com fs.ModeIrregular. É o caso dos
// arquivos do OneDrive (IO_REPARSE_TAG_CLOUD_*, inclusive os "sempre manter
// neste dispositivo"). O walker pulava tudo o que não era regular, calado: com
// a Área de Trabalho e os Documentos redirecionados para o OneDrive — o padrão
// do Windows 11 —, o backup dessas pastas saía vazio e "concluído".
//
// Escolha: tratar aqui, só no Windows, em vez de `//go:debug winsymlink=0` no
// main. O godebug volta o programa inteiro ao comportamento de antes do 1.23
// (junções viram links simbólicos também no navegador de pastas e na
// restauração), e o Go avisa que essas chaves de compatibilidade saem um dia.
//
// Só entra o que classificarReparse aceita pela tag (Cloud Files API) e que
// tem o conteúdo no disco, e os arquivos WOF e dedup que abrem pela leitura
// comum. O AppExecLink, link e junção ficam de fora calados;
// os outros reparse points (Azure File Sync, HSM, filtros de terceiros) e os
// que não deu para ler voltam reparsePulado, para o walker contar. O que é de
// nuvem mas só está na nuvem volta reparseSoNaNuvem, para o walker contar sem
// baixar.
func irregularLegivel(windows bool, path string, modo fs.FileMode) (fs.FileInfo, classeReparse) {
	if !windows || modo&fs.ModeIrregular == 0 || modo.IsDir() {
		return nil, reparseIgnorar
	}
	atributos, tag, err := lerReparse(path)
	if err != nil {
		// Sem a tag não há como saber se é dado do usuário: conta, a menos
		// que tenha sido apagado no meio da leitura.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, reparseIgnorar
		}
		return nil, reparsePulado
	}
	classe := classificarReparse(atributos, tag)
	if classe == reparseLegivel {
		// WOF/dedup: entra se a leitura comum abre o arquivo. Se não abre,
		// não é "removido na origem": conta como pulado.
		f, err := abrirParaLer(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, reparseIgnorar
			}
			return nil, reparsePulado
		}
		_ = f.Close()
		classe = reparseCopiar
	}
	if classe != reparseCopiar {
		return nil, classe
	}
	st, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, reparsePulado
	}
	if err != nil || st.IsDir() {
		return nil, reparseIgnorar
	}
	// O próprio Stat do Go devolve de novo ModeIrregular para esses reparse
	// points (ele não os segue); o que importa é não ser pasta, pipe nem
	// dispositivo.
	if st.Mode().Type()&^fs.ModeIrregular != 0 {
		return nil, reparseIgnorar
	}
	return st, reparseCopiar
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

// excluido diz se p, dentro da pasta do plano raiz, casa com algum padrão de
// exclusão: pelo nome (*.tmp) ou por um segmento do caminho (node_modules).
//
// Só conta o caminho RELATIVO à pasta do plano. Era o caminho absoluto: um
// segmento da própria pasta escolhida excluía tudo — AppData em
// C:\Users\Ana\AppData\Roaming\Thunderbird, cache em /var/cache/app, e até o
// host de /host no Docker. A pasta do plano em si nunca é excluída.
//
// Função pura, com o separador pela plataforma dada, para os testes cobrirem
// caminhos do Windows em qualquer sistema. No Windows os nomes não
// diferenciam maiúsculas (os padrões já vêm em minúsculas e com `\` de
// prepararExclusoes).
func excluido(windows bool, raiz, p string, globs []string) bool {
	if len(globs) == 0 {
		return false
	}
	sep := "/"
	if windows {
		sep = `\`
	}
	rel := strings.TrimLeft(strings.TrimPrefix(p, raiz), sep)
	if rel == "" {
		return false
	}
	if windows {
		rel = strings.ToLower(rel)
	}
	base := rel[strings.LastIndex(rel, sep)+1:]
	comSep := sep + rel
	for _, g := range globs {
		// path.Match trata `\` como escape: no Windows, um padrão com `\` é
		// de vários segmentos e nunca casa com um nome só.
		if !windows || !strings.Contains(g, `\`) {
			if ok, _ := path.Match(g, base); ok {
				return true
			}
		}
		// Segmento (ou sequência de segmentos) do caminho relativo.
		if strings.Contains(comSep, sep+g+sep) || strings.HasSuffix(comSep, sep+g) {
			return true
		}
	}
	return false
}
