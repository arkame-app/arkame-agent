//go:build windows

package restore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
	"golang.org/x/sys/windows"
)

// pasta é a pasta da restauração. No Windows não há openat: a conferência é
// por caminho, componente por componente, e o CreateTemp, o rename e a data
// remontam o caminho em texto. Uma junção trocada no caminho depois da
// conferência (por exemplo enquanto o jaRestaurado lê um arquivo grande)
// desviaria a gravação, como SYSTEM, para C:\Windows\System32.
//
// Por isso a pasta fica aberta (handle h, sem FILE_SHARE_DELETE quando dá:
// ninguém a renomeia nem a apaga enquanto a restauração corre) e real é o
// caminho real dela, pelo GetFinalPathNameByHandle. Cada arquivo que a
// restauração cria, lê para o hash, data ou renomeia tem o caminho real
// conferido pelo handle dele, e a pasta tem de continuar em real (uma pasta
// acima renomeada a tira de lá): fora disso é ErrDestinoLink. Entre a última
// conferência e o MoveFileEx do rename sobra a janela de uma chamada.
type pasta struct {
	caminho string
	real    string
	h       windows.Handle
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
// herdam a ACL dela, como os vizinhos. No lugar de origem (noLugar), nada
// nasce com a DACL do segredo: a pasta original que tinha sido apagada
// (C:\Users\ana\proj) volta herdando a ACL da mãe, e a Ana a abre.
func abrirPasta(_ string, destDir, subDir string, noLugar bool) (*pasta, error) {
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
			if merr == nil && i < nDestino && !protegida && !noLugar {
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
	return abrirHandleDaPasta(atual)
}

// FILE_NAME_NORMALIZED | VOLUME_NAME_DOS do GetFinalPathNameByHandle (o
// x/sys/windows não tem as constantes).
const nomeNormalizadoDOS = 0

// caminhoReal é o caminho real do arquivo ou pasta do handle, com os links e
// as junções do caminho resolvidos.
func caminhoReal(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), nomeNormalizadoDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// abrirHandleDaPasta abre a pasta conferida, sem seguir um reparse point que
// tenha aparecido no lugar dela, e confere que o caminho real é o caminho
// conferido: uma pasta acima trocada por junção entre a conferência e a
// abertura muda o caminho real.
func abrirHandleDaPasta(caminho string) (*pasta, error) {
	p, err := windows.UTF16PtrFromString(caminho)
	if err != nil {
		return nil, err
	}
	// FILE_LIST_DIRECTORY, e não só atributos: sem acesso a dados o Windows
	// não registra o modo de compartilhamento, e a pasta continuaria
	// renomeável. Se outro já a tem aberta para apagar, segue sem a trava;
	// a conferência do caminho real continua valendo.
	abrir := func(compartilhar uint32) (windows.Handle, error) {
		return windows.CreateFile(p, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES,
			compartilhar, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	}
	h, err := abrir(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		h, err = abrir(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	}
	if err != nil {
		return nil, fmt.Errorf("abrindo %s: %w", caminho, err)
	}
	real, err := caminhoReal(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("caminho real de %s: %w", caminho, err)
	}
	if !mesmoCaminhoWindows(real, caminho) {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%w: %s leva a %s", ErrDestinoLink, caminho, semPrefixoLongo(real))
	}
	return &pasta{caminho: caminho, real: real, h: h}, nil
}

// conferirNaPasta: o arquivo do handle está, de fato, em real com o nome dado.
func (p *pasta) conferirNaPasta(h windows.Handle, nome string) error {
	real, err := caminhoReal(h)
	if err != nil {
		return fmt.Errorf("caminho real de %s: %w", nome, err)
	}
	if !arquivoNaPastaWindows(real, p.real, nome) {
		return fmt.Errorf("%w: %s foi parar em %s", ErrDestinoLink, filepath.Join(p.caminho, nome), semPrefixoLongo(real))
	}
	return nil
}

// noLugar confere que a pasta aberta continua onde foi conferida: se uma
// pasta acima foi renomeada (para pôr uma junção no lugar), o caminho real do
// handle muda, e o caminho em texto já não leva a ela.
func (p *pasta) noLugar() error {
	agora, err := caminhoReal(p.h)
	if err != nil {
		return fmt.Errorf("caminho real de %s: %w", p.caminho, err)
	}
	if !mesmoCaminhoWindows(agora, p.real) {
		return fmt.Errorf("%w: %s foi movida para %s", ErrDestinoLink, p.caminho, semPrefixoLongo(agora))
	}
	return nil
}

// abrirConferido abre nome na pasta (sem seguir reparse point no nome) com o
// acesso dado e confere o caminho real dele.
func (p *pasta) abrirConferido(nome string, acesso uint32) (windows.Handle, error) {
	if err := p.noLugar(); err != nil {
		return 0, err
	}
	u, err := windows.UTF16PtrFromString(filepath.Join(p.caminho, nome))
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(u, acesso,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	if err := p.conferirNaPasta(h, nome); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
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

func (p *pasta) Close() error { return windows.CloseHandle(p.h) }

// dono: no Windows o dono vem da ACL herdada da pasta (copiarDono não faz
// nada).
func (p *pasta) dono() (int, int, error) { return -1, -1, nil }

// criarTemp cria o temporário e confere, pelo handle dele, que ele nasceu na
// pasta conferida; se nasceu em outro lugar (junção trocada no caminho), é
// apagado de lá e a restauração para com ErrDestinoLink.
func (p *pasta) criarTemp(padrao string) (*os.File, string, error) {
	f, err := os.CreateTemp(p.caminho, padrao)
	if err != nil {
		return nil, "", err
	}
	nome := filepath.Base(f.Name())
	if err := p.conferirNaPasta(windows.Handle(f.Fd()), nome); err != nil {
		real, _ := caminhoReal(windows.Handle(f.Fd()))
		f.Close()
		if real != "" {
			_ = os.Remove(real) // o temporário é nosso: nome aleatório, criado agora
		}
		return nil, "", err
	}
	return f, nome, nil
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
	if err := p.conferirNaPasta(windows.Handle(f.Fd()), nome); err != nil {
		f.Close()
		return nil, infoArquivo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, infoArquivo{}, err
	}
	return f, infoDeFileInfo(st), nil
}

// remover apaga nome na pasta onde ela está agora (pelo caminho real do
// handle), não pelo caminho em texto, que pode levar a outro lugar.
func (p *pasta) remover(nome string) {
	agora, err := caminhoReal(p.h)
	if err != nil {
		return
	}
	_ = os.Remove(strings.TrimRight(agora, `\`) + `\` + nome)
}

// renomear confere de novo, logo antes, que o temporário está na pasta
// conferida, e renomeia pelo caminho real dela.
func (p *pasta) renomear(de, para string) error {
	h, err := p.abrirConferido(de, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	base := strings.TrimRight(p.real, `\`) + `\`
	return os.Rename(base+de, base+para)
}

// sincronizar não faz nada no Windows: o NTFS registra o rename no journal
// dele, e uma pasta não se abre para FlushFileBuffers.
func (p *pasta) sincronizar() error { return nil }

func (p *pasta) aplicarData(nome string, acesso, modificacao time.Time) error {
	h, err := p.abrirConferido(nome, windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return definirData(h, acesso, modificacao)
}
