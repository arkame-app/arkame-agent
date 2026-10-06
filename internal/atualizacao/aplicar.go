package atualizacao

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Pedido é a atualização que o painel mandou na resposta do heartbeat (ou que
// o comando `update` buscou no painel).
type Pedido struct {
	// Versao alvo, X.Y.Z.
	Versao string
	// Sha256 do programa deste sistema, segundo o painel. É o segundo canal:
	// tem de ser igual ao do checksums.txt assinado.
	Sha256 string
	// BaseURL de onde baixar (<base>/v<versão>/<arquivo>). Vazio: só a reserva.
	BaseURL string
	// Arquivo é o nome no release; vazio, o deste sistema (NomeNoRelease).
	Arquivo string
	// Forcar tenta de novo uma versão que esta máquina já pôs e reverteu.
	Forcar bool
	// PermitirReversao aceita versão menor ou igual à atual. Só o comando
	// manual a liga; o painel não tem como pedir.
	PermitirReversao bool
}

// Opcoes é o que a troca precisa saber desta instalação.
type Opcoes struct {
	// Programa é o arquivo a trocar (o que o serviço executa).
	Programa string
	// VersaoAtual é a deste processo.
	VersaoAtual string
	// Chaves aceitas na assinatura do release (ChavesConfiaveis).
	Chaves []ed25519.PublicKey
	// Cliente do download; nil usa ClientePadrao.
	Cliente *http.Client
	// Reserva é a segunda origem do download (ReleasesNoGitHub). Vazia: só o
	// endereço do pedido.
	Reserva string
	// ArgsDoAutoTeste são os argumentos com que o programa novo é executado
	// antes da troca (self-test --config …).
	ArgsDoAutoTeste []string
	// PrepararNovo deixa o programa novo com o dono e as permissões do atual
	// antes de ele ser executado (service.PrepararProgramaNovo). nil: nada.
	PrepararNovo func(atual, novo string) error
	// PeloLancador: a troca é no volume do Docker (lancador.go).
	PeloLancador bool
	// Executar roda o auto-teste; nil usa o os/exec. Os testes o trocam.
	Executar func(ctx context.Context, programa string, args ...string) ([]byte, error)
}

// Códigos de falha, como vão ao painel (update.error no heartbeat).
const (
	FalhaPedidoInvalido   = "invalid_request"
	FalhaVersaoDeDev      = "dev_build"
	FalhaSemChaves        = "no_trusted_keys"
	FalhaNaoEMaior        = "not_newer"
	FalhaJaRevertida      = "rolled_back_before"
	FalhaDownload         = "download_failed"
	FalhaAssinatura       = "signature_invalid"
	FalhaResumoDoPainel   = "panel_sha_mismatch"
	FalhaResumoDoArquivo  = "checksum_mismatch"
	FalhaPermissoes       = "permissions"
	FalhaAutoTeste        = "self_test_failed"
	FalhaVersaoDoPrograma = "version_mismatch"
	FalhaTroca            = "swap_failed"
)

// Falha é a troca que não aconteceu: o programa no lugar continua o mesmo.
type Falha struct {
	Codigo string
	Err    error
}

func (f *Falha) Error() string { return f.Codigo + ": " + f.Err.Error() }
func (f *Falha) Unwrap() error { return f.Err }

func falha(codigo string, formato string, args ...any) *Falha {
	return &Falha{Codigo: codigo, Err: fmt.Errorf(formato, args...)}
}

// Adiada é a troca que fica para depois, sem ser falha: outra troca em curso
// nesta máquina, ou a do agente vizinho ainda em prova.
type Adiada struct{ Motivo string }

func (a *Adiada) Error() string { return "atualização adiada: " + a.Motivo }

// Motivos de adiamento, como vão ao painel.
const (
	AdiadaOutraTroca    = "another_update_running"
	AdiadaVizinhoProva  = "sibling_proving"
	AdiadaBackup        = "backup_running"
	AdiadaRestauracao   = "restore_running"
	AdiadaExpurgo       = "purge_running"
	AdiadaServicoSaindo = "stopping"
)

// PrazoDaTroca limita a troca inteira (download, auto-teste e rename). A
// trava do bucket fica presa enquanto ela dura: um download pendurado não
// pode segurar o backup da noite.
const PrazoDaTroca = 15 * time.Minute

// prazoDoAutoTeste é quanto o programa novo tem para carregar a configuração,
// dar um heartbeat e consultar o bucket.
var prazoDoAutoTeste = 3 * time.Minute

// NomeNoRelease é o nome do programa avulso deste sistema no release
// (arkame-agent_linux_amd64, arkame-agent_windows_amd64.exe).
func NomeNoRelease() string {
	nome := "arkame-agent_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		nome += ".exe"
	}
	return nome
}

// Resultado de uma troca feita.
type Resultado struct {
	Versao string
	// JaNoLugar: o programa desta versão já estava no lugar (outro agente da
	// máquina, que divide o programa, fez a troca e ela passou na prova). Não
	// houve download: basta este processo reiniciar.
	JaNoLugar bool
}

// Aplicar baixa, confere e põe no lugar o programa do pedido. Devolve nil
// quando o programa novo está no lugar — o chamador então sai, para o
// gerenciador de serviço subir o novo. *Falha: nada mudou. *Adiada: nada
// mudou, e vale tentar de novo depois.
//
// As conferências, todas obrigatórias e nesta ordem: a versão é maior que a
// atual; o checksums.txt do release está assinado por uma chave embutida, para
// esta versão; o resumo do programa no checksums.txt é o que o painel mandou;
// o arquivo baixado tem esse resumo; o programa novo passa no próprio
// auto-teste e diz ser da versão pedida.
func Aplicar(ctx context.Context, p Pedido, op Opcoes) (*Resultado, error) {
	alvo, err := LerVersao(p.Versao)
	if err != nil {
		return nil, falha(FalhaPedidoInvalido, "%w", err)
	}
	resumoDoPainel := strings.ToLower(strings.TrimSpace(p.Sha256))
	if b, err := hex.DecodeString(resumoDoPainel); err != nil || len(b) != 32 {
		return nil, falha(FalhaPedidoInvalido, "o pedido não traz o SHA-256 do programa")
	}
	atual, err := LerVersao(op.VersaoAtual)
	if err != nil {
		return nil, falha(FalhaVersaoDeDev, "este programa (%q) não é uma versão publicada e não se atualiza sozinho", op.VersaoAtual)
	}
	if !alvo.Maior(atual) && !p.PermitirReversao {
		return nil, falha(FalhaNaoEMaior, "a versão %s não é maior que a atual (%s)", alvo, atual)
	}
	if len(op.Chaves) == 0 {
		return nil, &Falha{Codigo: FalhaSemChaves, Err: ErrSemChaves}
	}
	if op.Programa == "" {
		return nil, falha(FalhaPedidoInvalido, "não sei qual programa trocar")
	}

	l := LugarDe(op.Programa)
	destravar, ok, err := l.travar()
	if err != nil {
		return nil, falha(FalhaPermissoes, "não consigo gravar na pasta do programa: %v", err)
	}
	if !ok {
		return nil, &Adiada{Motivo: AdiadaOutraTroca}
	}
	defer destravar()

	// O que esta máquina já sabe desta versão.
	if e, tem := l.lerEstado(); tem && mesmaVersao(e.Versao, p.Versao) {
		switch e.Situacao {
		case SituacaoRevertida, SituacaoAbandonada:
			if !p.Forcar {
				return nil, falha(FalhaJaRevertida,
					"a versão %s já foi posta nesta máquina e voltou atrás (%s)", alvo, e.Motivo)
			}
		case SituacaoEmProva, SituacaoConfirmada:
			if noLugar, _ := resumoDoArquivo(l.Atual); noLugar == e.Sha256 && noLugar == resumoDoPainel {
				if e.Situacao == SituacaoEmProva {
					return nil, &Adiada{Motivo: AdiadaVizinhoProva}
				}
				return &Resultado{Versao: alvo.String(), JaNoLugar: true}, nil
			}
		}
	}

	cliente := op.Cliente
	if cliente == nil {
		cliente = ClientePadrao()
	}
	bases := basesDaVersao(p.BaseURL, op.Reserva, alvo)
	if len(bases) == 0 {
		return nil, falha(FalhaDownload, "sem endereço https de onde baixar a versão %s", alvo)
	}
	nome := p.Arquivo
	if nome == "" {
		nome = NomeNoRelease()
	}
	if strings.ContainsAny(nome, `/\`) || strings.HasPrefix(nome, ".") {
		return nil, falha(FalhaPedidoInvalido, "nome de arquivo inválido no pedido: %q", nome)
	}

	// 1. O checksums.txt assinado, para esta versão.
	checksums, err := buscar(ctx, cliente, bases, ArquivoDeChecksums)
	if err != nil {
		return nil, falha(FalhaDownload, "baixando o %s: %w", ArquivoDeChecksums, err)
	}
	assinatura, err := buscar(ctx, cliente, bases, ArquivoDaAssinatura)
	if err != nil {
		return nil, falha(FalhaDownload, "baixando o %s: %w", ArquivoDaAssinatura, err)
	}
	if err := VerificarAssinatura(op.Chaves, alvo, checksums, assinatura); err != nil {
		return nil, &Falha{Codigo: FalhaAssinatura, Err: err}
	}
	// 2. Os dois canais dizem o mesmo resumo.
	resumoAssinado, err := ResumoNoChecksums(checksums, nome)
	if err != nil {
		return nil, falha(FalhaResumoDoPainel, "%w", err)
	}
	if resumoAssinado != resumoDoPainel {
		return nil, falha(FalhaResumoDoPainel,
			"o painel mandou o SHA-256 %s para %s, e o checksums.txt assinado diz %s", resumoDoPainel, nome, resumoAssinado)
	}

	// 3. O arquivo baixado é esse.
	baixado, err := baixarPara(ctx, cliente, bases, nome, l.Novo)
	if err != nil {
		return nil, falha(FalhaDownload, "%w", err)
	}
	feito := false
	defer func() {
		if !feito {
			_ = os.Remove(l.Novo)
		}
	}()
	if baixado != resumoAssinado {
		return nil, falha(FalhaResumoDoArquivo,
			"o arquivo baixado tem o SHA-256 %s, e o checksums.txt assinado diz %s", baixado, resumoAssinado)
	}
	if err := os.Chmod(l.Novo, 0o755); err != nil {
		return nil, falha(FalhaPermissoes, "%w", err)
	}
	if op.PrepararNovo != nil {
		if err := op.PrepararNovo(l.Atual, l.Novo); err != nil {
			return nil, falha(FalhaPermissoes, "%w", err)
		}
	}

	// 4. O programa novo roda aqui, com esta configuração, e é da versão pedida.
	if err := autoTestar(ctx, op, l.Novo, alvo); err != nil {
		return nil, err
	}

	// 5. A troca. O registro vai ao disco antes: se o processo morrer logo
	// depois do rename, o programa novo já sobe em prova.
	anterior, tinhaEstado := l.lerEstado()
	novoEstado := Estado{
		Situacao:       SituacaoEmProva,
		Versao:         alvo.String(),
		VersaoAnterior: atual.String(),
		Sha256:         baixado,
		AplicadaEm:     time.Now().UTC(),
		PeloLancador:   op.PeloLancador,
	}
	desfazerEstado := func() {
		if tinhaEstado {
			_ = l.gravarEstado(anterior)
		} else {
			l.apagarEstado()
		}
	}
	if op.PeloLancador {
		// O lançador confere o programa do volume a cada partida, sem rede.
		arqChecksums, arqAssinatura := l.arquivosDaVersao(alvo)
		if err := gravarArquivo(arqChecksums, checksums); err != nil {
			return nil, falha(FalhaTroca, "guardando o checksums.txt: %v", err)
		}
		if err := gravarArquivo(arqAssinatura, assinatura); err != nil {
			return nil, falha(FalhaTroca, "guardando a assinatura: %v", err)
		}
	}
	if err := l.gravarEstado(novoEstado); err != nil {
		return nil, falha(FalhaTroca, "gravando o registro da troca: %v", err)
	}
	if err := l.trocar(); err != nil {
		desfazerEstado()
		return nil, falha(FalhaTroca, "pondo o programa novo no lugar: %v", err)
	}
	feito = true
	return &Resultado{Versao: alvo.String()}, nil
}

// LinhaDaVersao é a linha que o `self-test` imprime com a versão do programa.
// A troca a confere: o programa novo tem de dizer que é da versão pedida.
const LinhaDaVersao = "versao="

// autoTestar executa o programa novo. Código de saída diferente de zero, ou
// programa que nem executa (no Windows com o Smart App Control ligado, o .exe
// sem assinatura de código é barrado), é falha: o programa atual fica.
func autoTestar(ctx context.Context, op Opcoes, novo string, alvo Versao) error {
	executar := op.Executar
	if executar == nil {
		executar = func(ctx context.Context, programa string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, programa, args...).CombinedOutput()
		}
	}
	tctx, cancel := context.WithTimeout(ctx, prazoDoAutoTeste)
	defer cancel()
	saida, err := executar(tctx, novo, op.ArgsDoAutoTeste...)
	if err != nil {
		return falha(FalhaAutoTeste, "o programa novo não passou no auto-teste: %v: %s", err, fimDaSaida(saida))
	}
	dita := ""
	for _, linha := range bytes.Split(saida, []byte("\n")) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(string(linha)), LinhaDaVersao); ok {
			dita = v
			break
		}
	}
	if !mesmaVersao(dita, alvo.String()) || dita == "" {
		return falha(FalhaVersaoDoPrograma, "o programa novo diz ser da versão %q, e o pedido é da %s", dita, alvo)
	}
	return nil
}

// fimDaSaida corta a saída do auto-teste para caber no log e no painel.
func fimDaSaida(saida []byte) string {
	s := strings.TrimSpace(string(saida))
	if len(s) > 600 {
		s = strings.ToValidUTF8(s[len(s)-600:], "")
	}
	return s
}

// CodigoDaFalha é o código que vai ao painel para um erro do Aplicar.
func CodigoDaFalha(err error) string {
	var f *Falha
	if errors.As(err, &f) {
		return f.Codigo
	}
	return "unexpected"
}
