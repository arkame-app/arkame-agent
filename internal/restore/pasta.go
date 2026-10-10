package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrDestinoLink: o caminho até a pasta da restauração passa por um link
// simbólico (no Windows, link ou junção) que não é do sistema. O daemon o
// relata ao painel com o código "dest_symlink".
//
// O agente grava como root (ou SYSTEM), e o operador do painel restaura para o
// lugar de origem ou para uma pasta nova dentro dele. Até 03/10 a gravação
// seguia os links do caminho: quem manda na pasta de origem (um usuário comum
// do servidor) a trocava, ou uma subpasta, por um link para /root/.ssh, e a
// restauração gravava lá — root no servidor.
var ErrDestinoLink = errors.New("o destino passa por um link simbólico")

// CodigoDestinoLink é o error_code do item recusado por ErrDestinoLink.
const CodigoDestinoLink = "dest_symlink"

// CodigoDestinoSomenteLeitura é o error_code do item cujo destino está num
// sistema de arquivos só de leitura (EROFS). No Linux, o serviço nativo
// (systemd, ProtectSystem=full) vê /etc, /usr e /boot só de leitura:
// restaurar no lugar original ali sempre falha, e o painel precisa dizer por
// quê, e não só "read-only file system". No Docker, o mesmo pedido funciona.
const CodigoDestinoSomenteLeitura = "read_only_destination"

// MensagemDestinoSomenteLeitura explica a falha por EROFS ao operador.
const MensagemDestinoSomenteLeitura = "o destino é só de leitura para o agente. No Linux, o serviço do agente " +
	"(systemd, ProtectSystem=full) não grava em /etc, /usr nem /boot: restaure em outra pasta e copie " +
	"de lá, ou use o agente em Docker"

// DestinoSomenteLeitura diz se a restauração falhou porque o destino é só de
// leitura (EROFS).
func DestinoSomenteLeitura(err error) bool { return errors.Is(err, syscall.EROFS) }

// ErrDestinoProtegido: o destino da restauração é a pasta do programa do
// agente, a da configuração dele, ou fica dentro de uma delas. O daemon o
// relata ao painel com o código "protected_destination".
//
// A atualização automática só põe no lugar um programa cujo checksums.txt tem
// a assinatura de release, e para trocar o programa o serviço do sistema
// ganhou escrita na pasta dele (ReadWritePaths, internal/service/systemd.go).
// A restauração grava como root onde o painel mandar: sem esta recusa, um
// item restaurado como `arkame-agent` (ou `arkame-agent.novo`, ou o registro
// da troca) na pasta do programa punha ali um programa que nenhuma assinatura
// cobre — e um `agent.env` restaurado em /etc/arkame trocava o painel e os
// comandos do agente. Quem compromete o painel não pode ganhar por aqui o que
// a assinatura lhe nega.
var ErrDestinoProtegido = errors.New("o destino é a pasta do programa ou da configuração do agente")

// CodigoDestinoProtegido é o error_code do item recusado por ErrDestinoProtegido.
const CodigoDestinoProtegido = "protected_destination"

// MensagemDestinoProtegido explica a recusa ao operador.
const MensagemDestinoProtegido = "o agente não restaura para dentro da pasta do próprio programa nem da própria " +
	"configuração (a atualização automática depende de só o agente gravar ali): restaure em outra pasta e copie de lá"

// dentroDeProtegida diz, só pelo texto do caminho, se alvo é uma das pastas
// protegidas ou fica dentro de uma. É a recusa barata, antes de criar pasta; a
// que vale contra link e caminho alternativo é a da pasta aberta (dentroDe).
func dentroDeProtegida(alvo string, protegidas []string) (string, bool) {
	alvo = filepath.Clean(alvo)
	for _, p := range protegidas {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		rel, err := filepath.Rel(p, alvo)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return p, true
		}
	}
	return "", false
}

// maxLinks limita a cadeia de links do sistema seguidos, como o kernel (ELOOP).
const maxLinks = 40

// infoArquivo é o que a restauração lê de um nome da pasta aberta, sem seguir
// link: se é arquivo comum, o tamanho, o modo (permissões e setuid, setgid e
// sticky) e o dono.
type infoArquivo struct {
	regular  bool
	tamanho  int64
	modo     os.FileMode
	uid, gid int
}

// noLugarDeOrigem diz se o item volta ao lugar de onde veio: o caminho final
// (destino + dest_filename), levado à chave do bucket (chaveFinal, como a do
// backup), é o fim da source_key — que é o prefixo do armazenamento seguido
// dessa mesma chave. No Windows, sem diferenciar maiúsculas.
//
// No Windows, decide a DACL das pastas que a restauração cria: a pasta de
// restauração nova (C:\Restaurados) nasce só para Administradores e SYSTEM,
// mas a pasta do lugar de origem que tinha sido apagada (C:\Users\ana\proj)
// volta herdando a ACL da mãe, como antes de apagada — com a DACL do segredo,
// a Ana perdia o acesso à própria pasta.
func noLugarDeOrigem(windows bool, chaveFinal, sourceKey string) bool {
	chaveFinal = strings.Trim(chaveFinal, "/")
	if chaveFinal == "" || chaveFinal == "." {
		return false
	}
	if windows {
		chaveFinal, sourceKey = strings.ToLower(chaveFinal), strings.ToLower(sourceKey)
	}
	return sourceKey == chaveFinal || strings.HasSuffix(sourceKey, "/"+chaveFinal)
}
