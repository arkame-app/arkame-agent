package restore

import "errors"

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
