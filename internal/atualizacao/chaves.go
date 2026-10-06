package atualizacao

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// As chaves públicas de release: Ed25519, 32 bytes, em base64 padrão (44
// caracteres). O agente só troca o próprio programa por um cujo checksums.txt
// foi assinado por uma delas.
//
// São duas posições para a troca de chave não exigir atualização manual: a
// próxima é embutida numa versão ANTES de passar a assinar; quando todos os
// agentes já a têm, ela vira a atual e a posição da próxima fica livre de
// novo. A chave privada nunca entra neste repositório — fica no environment
// `release` do GitHub (ver docs/RELEASE-ASSINATURA.md, que traz o passo a
// passo de gerar, guardar e trocar).
//
// Vazia, a posição não vale. Com as duas vazias este programa não confia em
// release nenhum e não se atualiza sozinho; o build de release (-tags
// release) nem compila com a atual vazia (chave_release.go).
const (
	chavePublicaAtual   = ""
	chavePublicaProxima = ""
)

// tamanhoDaChaveEmBase64 é o de 32 bytes em base64 padrão, com o "=".
const tamanhoDaChaveEmBase64 = 44

// ChavesConfiaveis devolve as chaves que este programa aceita na assinatura
// de um release. No build de desenvolvimento com a chave de teste habilitada
// (chave_teste.go), só ela; fora disso, as de produção que estiverem
// preenchidas.
func ChavesConfiaveis() []ed25519.PublicKey {
	textos := chavesDeTeste()
	if len(textos) == 0 {
		textos = []string{chavePublicaAtual, chavePublicaProxima}
	}
	var chaves []ed25519.PublicKey
	for _, t := range textos {
		if c, err := LerChavePublica(t); err == nil {
			chaves = append(chaves, c)
		}
	}
	return chaves
}

// LerChavePublica decodifica uma chave pública Ed25519 em base64 padrão.
func LerChavePublica(texto string) (ed25519.PublicKey, error) {
	texto = strings.TrimSpace(texto)
	if texto == "" {
		return nil, fmt.Errorf("chave pública vazia")
	}
	b, err := base64.StdEncoding.DecodeString(texto)
	if err != nil {
		return nil, fmt.Errorf("chave pública não é base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("chave pública com %d bytes, e não %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
