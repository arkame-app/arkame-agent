//go:build !windows

package restore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// pasta é a pasta da restauração, aberta sem seguir link de usuário. As
// gravações vão pelo descritor (openat, renameat): trocar uma pasta do caminho
// por um link depois da conferência não muda onde o arquivo cai.
type pasta struct {
	caminho string // para leitura e mensagens
	fd      int
}

// euid é o usuário do agente; variável para o teste simular o agente como root.
var euid = os.Geteuid

// abrirPasta abre (criando o que falta) a pasta destDir/subDir dentro do
// hostRoot, um componente por vez, com O_NOFOLLOW. O hostRoot em si (o /host
// do Docker) é aberto como está.
//
// Um link no caminho só é seguido se for do sistema: dele e da pasta onde está
// é dono o root (ou o próprio agente, quando não roda como root), e a pasta não
// aceita escrita do grupo nem dos outros. É o caso de /home -> var/home no
// Fedora Atomic e de /var -> private/var no macOS; nenhum usuário comum
// consegue plantar um assim. Qualquer outro link é ErrDestinoLink. Como em
// caminho.Real, um link absoluto recomeça no hostRoot.
//
// As pastas criadas ficam 0700, do dono da pasta-mãe (herdarDonoDaMae), no
// lugar de origem ou não: o último argumento (noLugarDeOrigem) só conta no
// Windows.
func abrirPasta(hostRoot, destDir, subDir string, _ bool) (*pasta, error) {
	raiz := filepath.Clean(hostRoot)
	if raiz == "" || raiz == "." {
		raiz = "/"
	}
	rel, err := filepath.Rel(raiz, destDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("destino %s fora de %s", destDir, raiz)
	}
	pendentes := append(partes(rel), partes(subDir)...)

	rootFd, err := unix.Open(raiz, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("abrindo %s: %w", raiz, err)
	}
	fds := []int{rootFd}     // fds[i] é a pasta de nomes[:i]
	var nomes []string       // caminho dentro do hostRoot
	fechar := func(de int) { // fecha fds[de:]
		for _, fd := range fds[de:] {
			unix.Close(fd)
		}
		fds = fds[:de]
	}
	ok := false
	defer func() {
		if !ok {
			fechar(0)
		}
	}()
	atual := func() string { return filepath.Join(append([]string{raiz}, nomes...)...) }

	seguidos, criados := 0, 0
	recemCriada := "" // a pasta que o Mkdirat acabou de criar, aberta na volta seguinte
	for len(pendentes) > 0 {
		c := pendentes[0]
		pendentes = pendentes[1:]
		switch c {
		case "", ".":
			continue
		case "..": // só vem do alvo de um link do sistema
			if len(nomes) > 0 {
				fechar(len(fds) - 1)
				nomes = nomes[:len(nomes)-1]
			}
			continue
		}
		dir := fds[len(fds)-1]
		fd, err := unix.Openat(dir, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		criada := c == recemCriada
		recemCriada = ""
		if err == nil {
			if criada {
				if herr := herdarDonoDaMae(dir, fd); herr != nil {
					unix.Close(fd)
					return nil, fmt.Errorf("chown %s: %w", filepath.Join(atual(), c), herr)
				}
			}
			fds = append(fds, fd)
			nomes = append(nomes, c)
			continue
		}
		if errors.Is(err, unix.ENOENT) && criados < 1000 {
			criados++
			// 0700: o que vai dentro pode ser segredo, e o modo original
			// da pasta não está no índice. O dono é o da pasta-mãe (ver
			// herdarDonoDaMae), depois de aberta.
			merr := unix.Mkdirat(dir, c, 0o700)
			if merr != nil && !errors.Is(merr, unix.EEXIST) {
				return nil, fmt.Errorf("mkdir %s: %w", filepath.Join(atual(), c), merr)
			}
			if merr == nil {
				recemCriada = c
			}
			// Abre na próxima volta, de novo sem seguir link: se alguém pôs
			// um link no lugar entre o mkdir e o open, cai na conferência.
			pendentes = append([]string{c}, pendentes...)
			continue
		}
		var st unix.Stat_t
		if serr := unix.Fstatat(dir, c, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
			return nil, fmt.Errorf("abrindo %s: %w", filepath.Join(atual(), c), err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFLNK {
			return nil, fmt.Errorf("abrindo %s: %w", filepath.Join(atual(), c), err)
		}
		if !linkDoSistema(dir, &st) || seguidos >= maxLinks {
			return nil, fmt.Errorf("%w: %s", ErrDestinoLink, filepath.Join(atual(), c))
		}
		alvo, err := lerLink(dir, c)
		if err != nil {
			return nil, fmt.Errorf("lendo o link %s: %w", filepath.Join(atual(), c), err)
		}
		seguidos++
		if filepath.IsAbs(alvo) {
			fechar(1)
			nomes = nil
		}
		pendentes = append(partes(alvo), pendentes...)
	}

	fd := fds[len(fds)-1]
	fds = fds[:len(fds)-1]
	fechar(0)
	ok = true
	return &pasta{caminho: atual(), fd: fd}, nil
}

// trocarDonoDaPasta é o fchown da pasta que a restauração criou; variável
// para o teste ver, sem root, o dono escolhido. Só como root, como o
// copiarDono: outro usuário não dá a pasta a terceiros.
var trocarDonoDaPasta = func(fd, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return unix.Fchown(fd, uid, gid)
}

// herdarDonoDaMae dá à pasta que a restauração acabou de criar (fd) o dono e
// o grupo da pasta-mãe aberta (maeFd). Criada pelo root, ela ficava root 0700:
// a Ana restaurava /home/ana/proj/relatorio.odt, a pasta proj apagada voltava
// do root, e a Ana não entrava na própria pasta. Dar a pasta ao dono da mãe é
// seguro: ele já pode criar pastas nela.
//
// Só se a pasta aberta ainda é a que o agente criou (do agente, sem acesso do
// grupo nem dos outros): entre o mkdir e o open, quem manda na mãe pode pôr
// outra pasta no lugar, e uma pasta alheia não muda de dono.
func herdarDonoDaMae(maeFd, fd int) error {
	var mae, st unix.Stat_t
	if err := unix.Fstat(maeFd, &mae); err != nil {
		return err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if int64(st.Uid) != int64(euid()) || st.Mode&0o077 != 0 {
		return nil
	}
	return trocarDonoDaPasta(fd, int(mae.Uid), int(mae.Gid))
}

// dono é o dono e o grupo da pasta aberta, pelo descritor.
func (p *pasta) dono() (int, int, error) {
	var st unix.Stat_t
	if err := unix.Fstat(p.fd, &st); err != nil {
		return 0, 0, &os.PathError{Op: "stat", Path: p.caminho, Err: err}
	}
	return int(st.Uid), int(st.Gid), nil
}

// linkDoSistema: o link e a pasta onde ele está são do root (ou do agente), e
// a pasta não aceita escrita do grupo nem dos outros.
func linkDoSistema(dirFd int, link *unix.Stat_t) bool {
	confiavel := func(uid uint32) bool { return uid == 0 || int64(uid) == int64(euid()) }
	if !confiavel(link.Uid) {
		return false
	}
	var d unix.Stat_t
	if err := unix.Fstat(dirFd, &d); err != nil {
		return false
	}
	return confiavel(d.Uid) && d.Mode&0o022 == 0
}

func lerLink(dirFd int, nome string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(dirFd, nome, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func partes(p string) []string {
	return strings.Split(strings.Trim(filepath.ToSlash(p), "/"), "/")
}

func (p *pasta) Close() error { return unix.Close(p.fd) }

// criarTemp cria o temporário na pasta com O_EXCL|O_NOFOLLOW (0600, como o
// os.CreateTemp). padrao tem um `*`, trocado por dígitos aleatórios.
func (p *pasta) criarTemp(padrao string) (*os.File, string, error) {
	antes, depois, _ := strings.Cut(padrao, "*")
	for range 100 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		nome := antes + hex.EncodeToString(b[:]) + depois
		fd, err := unix.Openat(p.fd, nome, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return os.NewFile(uintptr(fd), filepath.Join(p.caminho, nome)), nome, nil
	}
	return nil, "", fmt.Errorf("sem nome livre para o temporário em %s", p.caminho)
}

// infoDe lê o nome dentro da pasta aberta (fstatat, sem seguir link). O
// caminho em texto não serve: quem manda numa pasta do caminho a troca por um
// link depois da abertura, e o stat por texto passaria a ler um arquivo de
// fora (o dono e o modo do /usr/bin/passwd, por exemplo).
func (p *pasta) infoDe(nome string) (infoArquivo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(p.fd, nome, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return infoArquivo{}, &os.PathError{Op: "stat", Path: filepath.Join(p.caminho, nome), Err: err}
	}
	return infoDeStat(&st), nil
}

func infoDeStat(st *unix.Stat_t) infoArquivo {
	m := uint32(st.Mode)
	modo := os.FileMode(m & 0o777)
	if m&unix.S_ISUID != 0 {
		modo |= os.ModeSetuid
	}
	if m&unix.S_ISGID != 0 {
		modo |= os.ModeSetgid
	}
	if m&unix.S_ISVTX != 0 {
		modo |= os.ModeSticky
	}
	return infoArquivo{
		regular: m&unix.S_IFMT == unix.S_IFREG,
		tamanho: st.Size,
		modo:    modo,
		uid:     int(st.Uid),
		gid:     int(st.Gid),
	}
}

// abrirLeitura abre o arquivo comum nome da pasta aberta, sem seguir link
// (O_NOFOLLOW) e sem travar num FIFO posto no lugar (O_NONBLOCK). Devolve
// também o que o fstat do descritor diz dele.
func (p *pasta) abrirLeitura(nome string) (*os.File, infoArquivo, error) {
	fd, err := unix.Openat(p.fd, nome, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, infoArquivo{}, &os.PathError{Op: "open", Path: filepath.Join(p.caminho, nome), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, infoArquivo{}, &os.PathError{Op: "stat", Path: filepath.Join(p.caminho, nome), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(p.caminho, nome)), infoDeStat(&st), nil
}

func (p *pasta) remover(nome string) { _ = unix.Unlinkat(p.fd, nome, 0) }

// renomear troca o nome dentro da pasta aberta; um link no nome final é
// substituído, não seguido.
func (p *pasta) renomear(de, para string) error {
	return unix.Renameat(p.fd, de, p.fd, para)
}

// sincronizar leva ao disco a entrada da pasta (o rename do temporário),
// pelo descritor já aberto.
func (p *pasta) sincronizar() error { return unix.Fsync(p.fd) }

func (p *pasta) aplicarData(nome string, acesso, modificacao time.Time) error {
	return aplicarData(p.fd, nome, acesso, modificacao)
}
