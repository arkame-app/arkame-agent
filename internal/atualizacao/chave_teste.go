//go:build !release

package atualizacao

// chavePublicaDeTeste habilita, num build de desenvolvimento, uma chave de
// teste no lugar das de produção:
//
//	go build -ldflags "-X github.com/arkame-app/agent/internal/atualizacao.chavePublicaDeTeste=<base64>" ./cmd/arkame-agent
//
// É o que o ensaio local usa (docs/RELEASE-ASSINATURA.md). Com ela, as chaves
// de produção deixam de valer neste programa: um build de ensaio não aceita
// release de verdade, nem o contrário. No build de release (-tags release)
// esta variável não existe e o -X não tem onde gravar.
var chavePublicaDeTeste string

func chavesDeTeste() []string {
	if chavePublicaDeTeste == "" {
		return nil
	}
	return []string{chavePublicaDeTeste}
}
