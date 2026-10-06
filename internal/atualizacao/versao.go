package atualizacao

import (
	"fmt"
	"strconv"
	"strings"
)

// Versao é uma versão publicada do agente: X.Y.Z, sem sufixo. O build de
// desenvolvimento ("dev", ou o `git describe` do Makefile, v0.4.31-3-gabc)
// não é uma versão publicada e não se compara com nada.
type Versao [3]int

// LerVersao aceita "0.4.31" e "v0.4.31". Qualquer outra forma é erro: uma
// versão que não dá para ordenar não pode decidir se a troca é para a frente.
func LerVersao(s string) (Versao, error) {
	var v Versao
	partes := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(partes) != 3 {
		return v, fmt.Errorf("versão %q não tem a forma X.Y.Z", s)
	}
	for i, p := range partes {
		// Só dígitos: o Atoi aceitaria "+1" e "-1".
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return v, fmt.Errorf("versão %q não tem a forma X.Y.Z", s)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("versão %q não tem a forma X.Y.Z", s)
		}
		v[i] = n
	}
	return v, nil
}

// Maior diz se v vem depois de outra.
func (v Versao) Maior(outra Versao) bool {
	for i := range v {
		if v[i] != outra[i] {
			return v[i] > outra[i]
		}
	}
	return false
}

func (v Versao) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// Publicada diz se s é uma versão de release (e não um build de
// desenvolvimento).
func Publicada(s string) bool {
	_, err := LerVersao(s)
	return err == nil
}
