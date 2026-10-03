//go:build windows

package restore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
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
func abrirPasta(_ string, destDir, subDir string) (*pasta, error) {
	alvo := filepath.Join(destDir, filepath.FromSlash(subDir))
	vol := filepath.VolumeName(alvo)
	atual := vol + `\`
	for _, c := range strings.Split(strings.Trim(alvo[len(vol):], `\`), `\`) {
		if c == "" || c == "." {
			continue
		}
		if c == ".." {
			return nil, fmt.Errorf("destino com ..: %s", alvo)
		}
		atual = filepath.Join(atual, c)
		st, err := os.Lstat(atual)
		if os.IsNotExist(err) {
			if merr := os.Mkdir(atual, 0o755); merr != nil && !os.IsExist(merr) {
				return nil, fmt.Errorf("mkdir %s: %w", atual, merr)
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
