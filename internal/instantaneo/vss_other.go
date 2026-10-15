//go:build !windows || !(amd64 || arm64)

package instantaneo

import (
	"context"
	"errors"
)

// Suportado: fora do Windows de 64 bits, não há instantâneo do volume.
const Suportado = false

// criarVSS não existe fora do Windows de 64 bits.
func criarVSS(context.Context, []string) (map[string]string, func(), error) {
	return nil, nil, errors.New("instantâneo de volume não suportado neste sistema")
}
