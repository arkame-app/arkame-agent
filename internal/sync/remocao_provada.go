package sync

import (
	"errors"
	"hash/maphash"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// pastasListadas guarda, de uma varredura, as pastas que foram abertas e
// listadas SEM erro — a base da prova de remoção em sessão parcial
// (contrato do índice em lotes, item 27).
//
// Numa varredura completa, "o estado conhecia e não foi visto" basta para
// dizer que o arquivo saiu. Numa parcial não: o que faltou pode estar atrás
// de uma pasta que não abriu. Aí a remoção só é afirmada com prova positiva:
// o arquivo não existe (lstat) E a pasta que o listaria foi lida inteira
// nesta varredura, sem ele.
//
// Guarda um resumo de 64 bits por pasta (8 bytes, e não o caminho): com
// centenas de milhares de pastas a memória não acompanha o tamanho dos nomes.
// A semente é sorteada por execução.
//
// O walker escreve e, só depois de ele terminar, o coletor lê: não há acesso
// simultâneo.
type pastasListadas struct {
	semente maphash.Seed
	vistas  map[uint64]struct{}
	fontes  []fonteDoPlano
}

// fonteDoPlano é uma pasta do plano: a chave dela no bucket (relativa ao
// prefixo do plano, sem barras nas pontas) e o caminho de verdade no disco.
type fonteDoPlano struct {
	chave string
	disco string
}

func novasPastasListadas() *pastasListadas {
	return &pastasListadas{semente: maphash.MakeSeed(), vistas: map[uint64]struct{}{}}
}

// chaveDePasta normaliza a chave de uma pasta: sem barras nas pontas, e a
// raiz do servidor como "".
func chaveDePasta(k string) string {
	k = strings.Trim(k, "/")
	if k == "." {
		return ""
	}
	return k
}

func (p *pastasListadas) resumo(chave string) uint64 {
	return maphash.String(p.semente, chaveDePasta(chave))
}

// fonte registra uma pasta do plano que a varredura vai percorrer.
func (p *pastasListadas) fonte(chave, disco string) {
	if p == nil {
		return
	}
	p.fontes = append(p.fontes, fonteDoPlano{chave: chaveDePasta(chave), disco: disco})
}

// entrou marca a pasta como aberta. O erro da listagem, se houver, chega
// depois (falhou), antes de qualquer filho.
func (p *pastasListadas) entrou(chave string) {
	if p != nil {
		p.vistas[p.resumo(chave)] = struct{}{}
	}
}

// falhou tira a pasta: a listagem dela deu erro (ou veio pela metade).
func (p *pastasListadas) falhou(chave string) {
	if p != nil {
		delete(p.vistas, p.resumo(chave))
	}
}

func (p *pastasListadas) listada(chave string) bool {
	_, ok := p.vistas[p.resumo(chave)]
	return ok
}

// lstatDaProva é o os.Lstat da prova de remoção; os testes o trocam.
var lstatDaProva = os.Lstat

// naoExiste diz se o caminho, com certeza, não existe. Qualquer outra
// resposta — existe, sem permissão, erro de disco, um arquivo no meio do
// caminho — não prova nada.
func naoExiste(caminho string) bool {
	_, err := lstatDaProva(caminho)
	return err != nil && errors.Is(err, fs.ErrNotExist)
}

// saiu diz se há prova positiva de que o arquivo de chave rel (relativa ao
// prefixo do plano) saiu da origem:
//
//  1. o lstat dele devolve "não existe"; e
//  2. a pasta-mãe foi listada sem erro nesta varredura — ou, se a pasta-mãe
//     também não existe mais (a pasta inteira foi apagada), a pasta listada
//     mais próxima acima dela não tem mais o filho que levava ao arquivo.
//
// Pasta que existe e não foi listada (sem permissão, erro de leitura, pasta
// do plano que não abriu) não prova nada do que está abaixo dela. Se a chave
// está sob mais de uma pasta do plano, a prova tem de valer em todas.
func (p *pastasListadas) saiu(rel string) bool {
	if p == nil {
		return false
	}
	achou := false
	for _, f := range p.fontes {
		dentro := rel
		if f.chave != "" {
			var ok bool
			dentro, ok = strings.CutPrefix(rel, f.chave+"/")
			if !ok {
				continue
			}
		}
		if dentro == "" || !p.saiuDe(f, dentro) {
			return false
		}
		achou = true
	}
	return achou
}

func (p *pastasListadas) saiuDe(f fonteDoPlano, dentro string) bool {
	partes := strings.Split(dentro, "/")
	disco := func(n int) string {
		return filepath.Join(append([]string{f.disco}, partes[:n]...)...)
	}
	if !naoExiste(disco(len(partes))) {
		return false
	}
	// Da pasta-mãe para cima, até a primeira que foi listada.
	for n := len(partes) - 1; n >= 0; n-- {
		chave := f.chave
		if n > 0 {
			chave = strings.TrimPrefix(f.chave+"/"+strings.Join(partes[:n], "/"), "/")
		}
		if !p.listada(chave) {
			continue
		}
		// A pasta listada tinha de mostrar partes[n]: se é o próprio arquivo,
		// o lstat acima já disse que não existe; se é uma pasta do caminho,
		// tem de ser ela que não existe mais.
		return n == len(partes)-1 || naoExiste(disco(n+1))
	}
	return false
}
