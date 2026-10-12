// Package segredo grava os arquivos sensíveis do agente — chave do bucket,
// token, chave privada, identidade — legíveis só pelo administrador.
//
// No Linux e no macOS isso é o modo do arquivo (0600). No Windows o modo não
// restringe nada: o os.WriteFile(…, 0600) deixava o token e a chave privada
// com a ACL herdada de C:\etc, legíveis por qualquer usuário da máquina. Lá a
// proteção é uma DACL só com Administradores e SYSTEM (ver proteger_windows.go),
// aplicada no arquivo e na pasta que o agente cria.
package segredo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Gravar escreve o arquivo protegido, por troca atômica: grava ao lado, protege
// e só então põe no lugar — o conteúdo nunca existe no disco sem a proteção, e
// um arquivo antigo com permissão larga (0644) é substituído, não reaproveitado
// (o os.WriteFile mantém a permissão de um arquivo que já existe).
//
// modo vale fora do Windows (0600 para segredo; 0644 para o que só precisa de
// integridade, como o agent.id). No Windows é sempre Administradores e SYSTEM.
func Gravar(caminho string, dados []byte, modo os.FileMode) error {
	if err := CriarPasta(filepath.Dir(caminho)); err != nil {
		return err
	}
	tmp := caminho + ".novo"
	if err := escreverSincronizado(tmp, dados); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	if err := protegerComModo(tmp, modo); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, caminho); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	SincronizarPasta(filepath.Dir(caminho))
	return nil
}

// escreverSincronizado grava o temporário e o leva ao disco (fsync) antes de
// fechar. Sem o fsync, o rename podia chegar ao disco antes dos dados (XFS, e
// ext4/btrfs conforme a montagem): depois de um corte de energia, o token
// renovado voltava com 0 bytes e o agente não se autenticava mais.
func escreverSincronizado(tmp string, dados []byte) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(dados); err != nil {
		f.Close()
		return err
	}
	if err := sincronizarArquivo(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sincronizarArquivo e sincronizarPasta são variáveis para o teste conferir a
// ordem: o arquivo vai ao disco antes do rename, a pasta depois.
var (
	sincronizarArquivo = (*os.File).Sync
	sincronizarPasta   = sincronizarPastaSO
)

// SincronizarPasta leva ao disco a entrada da pasta depois de um rename, para
// a troca sobreviver a um corte de energia. Fora do Windows abre a pasta e
// chama fsync; no Windows não faz nada. É o melhor possível: o rename já foi
// feito e, se a pasta não sincronizar (há sistemas de arquivos que recusam
// fsync em pasta), o pior caso é voltar o arquivo anterior, inteiro.
func SincronizarPasta(dir string) { _ = sincronizarPasta(dir) }

// Proteger deixa um arquivo que já existe legível só pelo administrador (0600;
// no Windows, Administradores e SYSTEM).
func Proteger(caminho string) error { return protegerComModo(caminho, 0o600) }

// ProtegerPasta deixa uma pasta que o agente acabou de criar só para o
// administrador, herdado pelo que for gravado nela: no Windows, a DACL
// protegida de Administradores e SYSTEM; fora dele não faz nada (quem cria
// a pasta já lhe dá o modo).
func ProtegerPasta(dir string) error { return protegerPasta(dir) }

// CriarPasta cria a pasta da configuração. Se fomos nós que a criamos, ela já
// nasce protegida (no Windows, Administradores e SYSTEM, herdado pelo que for
// gravado nela); pasta que já existia não é tocada — pode ser uma pasta da
// pessoa, apontada por --config.
func CriarPasta(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criando %s: %w", dir, err)
	}
	return protegerPasta(dir)
}

// ErrDonoNaoConfiavel: a pasta (ou o arquivo) já existia e pertence a alguém
// que não é o administrador, o sistema nem o usuário deste processo.
var ErrDonoNaoConfiavel = errors.New("dono não confiável")

// donoDeConfianca é o donoConfiavel do sistema; os testes o trocam.
var donoDeConfianca = donoConfiavel

// ConferirDono devolve ErrDonoNaoConfiavel se caminho pertence a alguém que
// não é de confiança: fora o root (no Windows, Administradores e SYSTEM) e o
// usuário deste processo. Quem cria uma pasta manda nela — pode trocar a
// permissão quando quiser e pôr lá dentro o que o agente vai ler. Um serviço
// que roda como root/SYSTEM não pode tratar como seu o que outro usuário
// deixou pronto no caminho.
func ConferirDono(caminho string) error {
	confiavel, dono, err := donoDeConfianca(caminho)
	if err != nil {
		return fmt.Errorf("conferindo o dono de %s: %w", caminho, err)
	}
	if !confiavel {
		return fmt.Errorf("%w: %s pertence a %s", ErrDonoNaoConfiavel, caminho, dono)
	}
	return nil
}

// PastaPrivada prepara dir para guardar o que só o administrador pode ler e
// alterar (o estado local do backup: os nomes, tamanhos, datas e sha256 de
// todos os arquivos do servidor, e o que decide se um arquivo é ou não lido).
//
//   - Se dir não existe, cria cada nível que faltar já protegido: 0700 fora do
//     Windows; no Windows, a DACL protegida de Administradores e SYSTEM,
//     herdada pelo que for gravado dentro. O modo do Mkdir não vale nada no
//     Windows: sem a DACL, a pasta herdava a de %ProgramData%, em que qualquer
//     usuário lê e cria arquivos. A pasta de cima (a primeira que já existe)
//     também tem de ser de dono confiável.
//   - Se dir já existe, tem de ser uma pasta de verdade (no Windows, não uma
//     junção nem um link) e de dono confiável (ConferirDono); senão, erro, e
//     quem chama usa outro lugar. A permissão de uma pasta que já existia não é
//     tocada — pode ser uma pasta escolhida pelo administrador, com outros
//     usos —; os arquivos gravados nela são protegidos um a um (Proteger).
func PastaPrivada(dir string) error {
	var faltam []string
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err == nil {
			if p == filepath.Clean(dir) {
				return pastaQueJaExistia(p, fi)
			}
			// Criar dentro da pasta de outro usuário não adianta: quem é dono
			// da pasta de cima renomeia a nossa e põe outra no lugar.
			if err := ConferirDono(p); err != nil {
				return err
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("criando %s: %w", dir, err)
		}
		faltam = append(faltam, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	for i := len(faltam) - 1; i >= 0; i-- {
		p := faltam[i]
		err := os.Mkdir(p, 0o700)
		if errors.Is(err, os.ErrExist) {
			// Alguém a criou entre a consulta e agora: não é nossa.
			fi, lerr := os.Lstat(p)
			if lerr != nil {
				return fmt.Errorf("criando %s: %w", dir, lerr)
			}
			if perr := pastaQueJaExistia(p, fi); perr != nil {
				return perr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("criando %s: %w", dir, err)
		}
		if err := protegerPasta(p); err != nil {
			return err
		}
	}
	return nil
}

func pastaQueJaExistia(p string, fi os.FileInfo) error {
	if err := pastaDeVerdade(p, fi); err != nil {
		return err
	}
	return ConferirDono(p)
}
