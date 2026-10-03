//go:build windows

package restore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
)

// pasta é a pasta da restauração. No Windows não há openat: a conferência é
// por caminho, componente por componente, antes de criar e de gravar.
type pasta struct {
	caminho string
}

// abrirPasta confere (criando o que falta) a pasta destDir/subDir, da raiz da
// unidade para baixo: um link simbólico ou uma junção (os reparse points que
// apontam para outro lugar — o Go os entrega sem ModeDir) no caminho é
// ErrDestinoLink. Pasta do OneDrive e outros reparse points que não desviam o
// caminho continuam valendo: chegam como pasta.
//
// A primeira pasta que o agente cria até destDir (C:\Restaurados, na
// restauração para pasta nova) nasce com a DACL do segredo: só
// Administradores e SYSTEM, herdada pelo que a restauração gravar nela. Sem
// isso ela herdava a ACL de C:\, que dá leitura a Users, e o que se restaura
// de um servidor (chaves, .env, bancos) ficava legível por qualquer usuário.
// As subpastas que o dest_filename cria dentro de uma pasta que já existia
// (restauração no lugar de origem) herdam a ACL dela, como os vizinhos.
func abrirPasta(_ string, destDir, subDir string) (*pasta, error) {
	alvo := filepath.Join(destDir, filepath.FromSlash(subDir))
	vol := filepath.VolumeName(alvo)
	atual := vol + `\`
	nDestino := len(componentes(destDir[len(filepath.VolumeName(destDir)):]))
	protegida := false
	for i, c := range componentes(alvo[len(vol):]) {
		if c == ".." {
			return nil, fmt.Errorf("destino com ..: %s", alvo)
		}
		atual = filepath.Join(atual, c)
		st, err := os.Lstat(atual)
		if os.IsNotExist(err) {
			merr := os.Mkdir(atual, 0o700)
			if merr != nil && !os.IsExist(merr) {
				return nil, fmt.Errorf("mkdir %s: %w", atual, merr)
			}
			if merr == nil && i < nDestino && !protegida {
				if perr := segredo.ProtegerPasta(atual); perr != nil {
					return nil, perr
				}
				protegida = true
			}
			st, err = os.Lstat(atual)
		}
		if err != nil {
			return nil, fmt.Errorf("abrindo %s: %w", atual, err)
		}
		if st.IsDir() {
			continue
		}
		if st.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return nil, fmt.Errorf("%w: %s", ErrDestinoLink, atual)
		}
		return nil, fmt.Errorf("%s não é pasta", atual)
	}
	return &pasta{caminho: atual}, nil
}

// componentes separa o caminho (sem a unidade) nas pastas, sem as vazias e
// sem ".".
func componentes(p string) []string {
	var cs []string
	for _, c := range strings.Split(strings.Trim(p, `\`), `\`) {
		if c != "" && c != "." {
			cs = append(cs, c)
		}
	}
	return cs
}

func (p *pasta) Close() error { return nil }

func (p *pasta) criarTemp(padrao string) (*os.File, string, error) {
	f, err := os.CreateTemp(p.caminho, padrao)
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Base(f.Name()), nil
}

// infoDe lê o nome dentro da pasta sem seguir link (Lstat).
func (p *pasta) infoDe(nome string) (infoArquivo, error) {
	st, err := os.Lstat(filepath.Join(p.caminho, nome))
	if err != nil {
		return infoArquivo{}, err
	}
	return infoDeFileInfo(st), nil
}

func infoDeFileInfo(st fs.FileInfo) infoArquivo {
	return infoArquivo{regular: st.Mode().IsRegular(), tamanho: st.Size(), modo: st.Mode().Perm()}
}

// abrirLeitura abre o arquivo nome da pasta para ler; devolve também o que o
// handle aberto diz dele.
func (p *pasta) abrirLeitura(nome string) (*os.File, infoArquivo, error) {
	f, err := os.Open(filepath.Join(p.caminho, nome))
	if err != nil {
		return nil, infoArquivo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, infoArquivo{}, err
	}
	return f, infoDeFileInfo(st), nil
}

func (p *pasta) remover(nome string) { _ = os.Remove(filepath.Join(p.caminho, nome)) }

func (p *pasta) renomear(de, para string) error {
	return os.Rename(filepath.Join(p.caminho, de), filepath.Join(p.caminho, para))
}

func (p *pasta) aplicarData(nome string, acesso, modificacao time.Time) error {
	return aplicarData(filepath.Join(p.caminho, nome), acesso, modificacao)
}
