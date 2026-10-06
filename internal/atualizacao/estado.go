package atualizacao

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
)

// Situações do registro de uma troca.
const (
	// O programa novo está no lugar e ainda não provou que funciona.
	SituacaoEmProva = "em_prova"
	// O programa novo teve um heartbeat aceito pelo painel.
	SituacaoConfirmada = "confirmada"
	// O programa novo não provou que funciona e o anterior voltou ao lugar.
	SituacaoRevertida = "revertida"
	// Docker: o programa do volume não provou que funciona (ou não é válido)
	// e o lançador deixou de usá-lo; vale o da imagem.
	SituacaoAbandonada = "abandonada"
)

// Estado é o registro da última troca, num arquivo ao lado do programa
// (Lugar.Estado). Fica ao lado do programa, e não da configuração, porque a
// troca é do programa: dois agentes da máquina que dividem o programa leem o
// mesmo registro.
type Estado struct {
	Situacao string `json:"situacao"`
	// Versao é a versão posta no lugar; VersaoAnterior, a que saiu.
	Versao         string    `json:"versao"`
	VersaoAnterior string    `json:"versao_anterior,omitempty"`
	Sha256         string    `json:"sha256"`
	AplicadaEm     time.Time `json:"aplicada_em"`
	// Arranques conta as partidas do programa novo sem sucesso, por agente
	// (a chave é o arquivo de configuração; no Docker, "lancador"). Zera
	// quando a prova passa.
	Arranques map[string]int `json:"arranques,omitempty"`
	// Motivo da reversão ou do abandono, para o painel.
	Motivo string `json:"motivo,omitempty"`
	// PeloLancador: a troca foi no volume do Docker. Não há programa anterior
	// para voltar: quem falha é abandonado e vale o programa da imagem.
	PeloLancador bool `json:"pelo_lancador,omitempty"`
}

// lerEstado devolve o registro, ou ok=false quando não há (ou está ilegível:
// um registro que não dá para ler não pode segurar nem reverter nada).
func (l Lugar) lerEstado() (Estado, bool) {
	var e Estado
	b, err := os.ReadFile(l.Estado)
	if err != nil {
		return e, false
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Situacao == "" || e.Versao == "" {
		return e, false
	}
	return e, true
}

// gravarEstado grava o registro por troca atômica, com fsync: ele decide, no
// próximo arranque, se o programa no lugar fica.
func (l Lugar) gravarEstado(e Estado) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return gravarArquivo(l.Estado, append(b, '\n'))
}

// gravarArquivo é o segredo.Gravar (troca atômica com fsync), legível por
// todos: nada aqui é segredo. Variável para os testes simularem o disco cheio.
var gravarArquivo = func(caminho string, dados []byte) error {
	return segredo.Gravar(caminho, dados, 0o644)
}

func (l Lugar) apagarEstado() {
	if err := os.Remove(l.Estado); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = err // fica para a próxima troca, que o sobrescreve
	}
}

// mesmaVersao compara duas versões pelo valor ("v0.5.0" e "0.5.0" são a
// mesma); o que não é versão publicada só é igual a si mesmo.
func mesmaVersao(a, b string) bool {
	va, ea := LerVersao(a)
	vb, eb := LerVersao(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return va == vb
}
