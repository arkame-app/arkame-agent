package restore

import (
	"errors"
	"os"
	"strings"
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
