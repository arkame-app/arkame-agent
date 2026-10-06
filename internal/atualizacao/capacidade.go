package atualizacao

import (
	"os"
	"path/filepath"
)

// Capacidade é o nome com que o agente declara ao painel, no heartbeat e no
// /sessions/start, que se atualiza sozinho. O painel só manda `update` a
// quem a declarou.
const Capacidade = "self_update"

// Impedimentos: por que este processo não se atualiza sozinho. Vão ao painel
// (update.blocked no heartbeat), que mostra o que fazer.
const (
	// ARKAME_AUTO_UPDATE=off.
	ImpedidaDesligada = "disabled"
	// Build de desenvolvimento, sem versão para comparar.
	ImpedidaVersaoDeDev = "dev_build"
	// Programa sem chave de release embutida.
	ImpedidaSemChaves = "no_trusted_keys"
	// Rodando à mão: ninguém religaria o processo depois da troca.
	ImpedidaSemServico = "no_service"
	// A pasta do programa não aceita escrita deste processo. É o serviço do
	// sistema no Linux instalado antes da atualização automática: basta rodar
	// o instalador mais uma vez, que regrava a unit.
	ImpedidaPastaSoLeitura = "binary_dir_readonly"
)

// Instalacao é o que decide se este processo pode se atualizar sozinho.
type Instalacao struct {
	// Programa a trocar: o que o serviço executa; no Docker, o do volume.
	Programa string
	// VersaoAtual deste processo.
	VersaoAtual string
	// Desligada: ARKAME_AUTO_UPDATE=off.
	Desligada bool
	// SobServico: há um gerenciador (systemd, launchd, SCM) que religa o
	// processo; no Docker, o lançador.
	SobServico bool
	// TemChaves: há chave de release embutida.
	TemChaves bool
}

// Impedimento devolve por que esta instalação não se atualiza sozinha, ou ""
// quando pode — e aí o agente declara a Capacidade.
func (i Instalacao) Impedimento() string {
	switch {
	case i.Desligada:
		return ImpedidaDesligada
	case !Publicada(i.VersaoAtual):
		return ImpedidaVersaoDeDev
	case !i.TemChaves:
		return ImpedidaSemChaves
	case !i.SobServico:
		return ImpedidaSemServico
	}
	if i.Programa == "" {
		return ImpedidaPastaSoLeitura
	}
	l := LugarDe(i.Programa)
	if !pastaExiste(l) || !l.PastaGravavel() {
		return ImpedidaPastaSoLeitura
	}
	return ""
}

// pastaExiste cria a pasta do programa do volume (no Docker ela não existe
// antes da primeira troca) e diz se há pasta.
func pastaExiste(l Lugar) bool {
	return criarPastaDoPrograma(l) == nil
}

var criarPastaDoPrograma = func(l Lugar) error {
	dir := filepath.Dir(l.Atual)
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return os.ErrInvalid
		}
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
