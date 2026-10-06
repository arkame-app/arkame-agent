package atualizacao

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// No Docker o programa da imagem não pode ser trocado (a camada é só de
// leitura, e trocar a imagem de dentro do container exigiria o socket do
// Docker). O programa novo vai para o volume da configuração, que persiste, e
// o programa da imagem vira um lançador: ao subir, executa o do volume se ele
// for válido e mais novo; se o do volume não provar que funciona, deixa de
// usá-lo e segue ele mesmo.

// Variáveis de ambiente do lançador.
const (
	// EnvAutoUpdate desliga a atualização automática com "off" (também
	// "false", "0", "no"). Vale no ambiente do container e no env-file.
	EnvAutoUpdate = "ARKAME_AUTO_UPDATE"
	// EnvLancador é o caminho do programa da imagem, que o lançador passa ao
	// programa do volume: é para ele que o programa do volume volta quando
	// precisa reiniciar. EnvVersaoDoLancador é a versão da imagem.
	EnvLancador         = "ARKAME_LANCADOR"
	EnvVersaoDoLancador = "ARKAME_LANCADOR_VERSAO"
	// envMetodo é a variável que o Dockerfile define ("docker").
	envMetodo = "ARKAME_INSTALL_METHOD"
	// chaveDoLancador conta as partidas no registro (Estado.Arranques).
	chaveDoLancador = "lancador"
)

// EmDocker diz se este processo roda na imagem do agente.
func EmDocker() bool { return os.Getenv(envMetodo) == "docker" }

// Desligada lê o valor de ARKAME_AUTO_UPDATE.
func Desligada(valor string) bool {
	switch strings.ToLower(strings.TrimSpace(valor)) {
	case "off", "false", "0", "no", "nao", "não":
		return true
	}
	return false
}

// ProgramaDoVolume é onde o programa baixado mora no Docker: bin/ ao lado do
// arquivo de configuração (/etc/arkame/bin/arkame-agent).
func ProgramaDoVolume(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "bin", "arkame-agent")
}

// EscolherDoVolume decide se o programa do volume deve ser executado no lugar
// do da imagem. Devolve o caminho, ou "" com o porquê.
//
// Só vale o programa que: tem registro de troca feita pelo lançador, não
// abandonada; é de versão maior que a da imagem (quem puxou uma imagem mais
// nova quer a imagem); confere, de novo e sem rede, com o checksums.txt
// assinado guardado ao lado dele; e, em prova, ainda não gastou as partidas.
// O que não confere é abandonado, para o painel saber e para não ser
// conferido a cada partida.
func EscolherDoVolume(programa, versaoDaImagem string, chaves []ed25519.PublicKey, nomeNoRelease string) (string, string) {
	l := LugarDe(programa)
	e, ok := l.lerEstado()
	if !ok || !e.PeloLancador {
		return "", "sem programa baixado no volume"
	}
	if e.Situacao != SituacaoEmProva && e.Situacao != SituacaoConfirmada {
		return "", "o programa do volume foi abandonado (" + e.Motivo + ")"
	}
	doVolume, err := LerVersao(e.Versao)
	if err != nil {
		return "", "registro com versão inválida"
	}
	daImagem, err := LerVersao(versaoDaImagem)
	if err != nil {
		return "", "a imagem é um build de desenvolvimento"
	}
	if !doVolume.Maior(daImagem) {
		return "", fmt.Sprintf("a imagem (%s) não é mais antiga que o programa do volume (%s)", daImagem, doVolume)
	}
	abandonar := func(motivo, detalhe string) (string, string) {
		e.Situacao = SituacaoAbandonada
		e.Motivo = motivo
		if err := l.gravarEstado(e); err != nil {
			slog.Warn("não consegui registrar o abandono do programa do volume", "err", err)
		}
		return "", detalhe
	}
	if err := conferirDoVolume(l, e, doVolume, chaves, nomeNoRelease); err != nil {
		return abandonar(MotivoInvalido, "o programa do volume não confere: "+err.Error())
	}
	if e.Situacao == SituacaoEmProva {
		if e.Arranques == nil {
			e.Arranques = map[string]int{}
		}
		e.Arranques[chaveDoLancador]++
		if e.Arranques[chaveDoLancador] >= MaximoDeArranques {
			return abandonar(MotivoArranques, "o programa do volume subiu três vezes sem um heartbeat aceito")
		}
		if err := l.gravarEstado(e); err != nil {
			// Sem conseguir contar, a prova não tem fim: não arrisca.
			return "", "não consegui contar a partida no registro: " + err.Error()
		}
	}
	return l.Atual, ""
}

// conferirDoVolume refaz as conferências da troca com o que está no disco.
func conferirDoVolume(l Lugar, e Estado, versao Versao, chaves []ed25519.PublicKey, nome string) error {
	arqChecksums, arqAssinatura := l.arquivosDaVersao(versao)
	checksums, err := os.ReadFile(arqChecksums)
	if err != nil {
		return err
	}
	assinatura, err := os.ReadFile(arqAssinatura)
	if err != nil {
		return err
	}
	if err := VerificarAssinatura(chaves, versao, checksums, assinatura); err != nil {
		return err
	}
	assinado, err := ResumoNoChecksums(checksums, nome)
	if err != nil {
		return err
	}
	noDisco, err := resumoDoArquivo(l.Atual)
	if err != nil {
		return err
	}
	if noDisco != assinado || noDisco != e.Sha256 {
		return fmt.Errorf("o SHA-256 do arquivo (%s) não é o do checksums.txt assinado (%s)", noDisco, assinado)
	}
	return nil
}

// AmbienteDoLancado é o ambiente do programa do volume: o do lançador, mais
// o caminho e a versão dele.
func AmbienteDoLancado(ambiente []string, lancador, versaoDoLancador string) []string {
	saida := AmbienteSemLancador(ambiente)
	return append(saida, EnvLancador+"="+lancador, EnvVersaoDoLancador+"="+versaoDoLancador)
}

// AmbienteSemLancador tira as variáveis do lançador: é o ambiente com que o
// programa do volume volta ao da imagem.
func AmbienteSemLancador(ambiente []string) []string {
	saida := make([]string, 0, len(ambiente)+2)
	for _, v := range ambiente {
		if strings.HasPrefix(v, EnvLancador+"=") || strings.HasPrefix(v, EnvVersaoDoLancador+"=") {
			continue
		}
		saida = append(saida, v)
	}
	return saida
}

// AbandonarDoVolume registra que o programa do volume não serve (não executou)
// e não deve ser tentado de novo; o painel recebe o motivo no heartbeat.
func AbandonarDoVolume(programa, motivo string) {
	l := LugarDe(programa)
	e, ok := l.lerEstado()
	if !ok {
		return
	}
	e.Situacao = SituacaoAbandonada
	e.Motivo = motivo
	if err := l.gravarEstado(e); err != nil {
		slog.Warn("não consegui registrar o abandono do programa do volume", "err", err)
	}
}
