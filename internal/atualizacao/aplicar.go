package atualizacao

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	// Arquivo é o nome no release segundo o painel. Só conferência: o arquivo
	// baixado é sempre o deste sistema (NomeNoRelease), e um nome diferente
	// recusa o pedido.
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
	FalhaArquivoDoPedido  = "file_mismatch"
	FalhaResumoDoArquivo  = "checksum_mismatch"
	FalhaPermissoes       = "permissions"
	FalhaAutoTeste        = "self_test_failed"
	FalhaAutoTesteBarrado = "self_test_blocked"
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

	// Uma troca anterior que parou entre os dois renames (ver Recuperar): o
	// programa volta ao lugar, e o registro lido a seguir já conta com isso.
	if feito := l.recuperar(); feito != "" {
		slog.Warn("uma troca anterior tinha parado no meio; pus o programa de volta no lugar", "programa", l.Atual, "qual", feito)
	}

	// O que esta máquina já sabe desta versão.
	if e, tem := l.lerEstado(); tem && mesmaVersao(e.Versao, p.Versao) {
		switch e.Situacao {
		case SituacaoRevertida, SituacaoAbandonada:
			if !p.Forcar {
				return nil, falha(FalhaJaRevertida,
					"a versão %s já foi posta nesta máquina e voltou atrás (%s)", alvo, e.Motivo)
			}
		case SituacaoEmProva, SituacaoConfirmada, SituacaoSemVolta:
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
	// O arquivo é sempre o deste sistema, pelo nome que ESTE programa sabe. O
	// nome que o painel manda só é conferido: aceito como veio, o painel
	// escolhia qualquer linha do checksums.txt assinado — o programa de outro
	// sistema, ou um pacote —, e a assinatura, que cobre o arquivo inteiro,
	// conferia.
	nome := NomeNoRelease()
	if p.Arquivo != "" && p.Arquivo != nome {
		return nil, falha(FalhaArquivoDoPedido, "o pedido traz o arquivo %q, e o programa deste sistema é %q", p.Arquivo, nome)
	}

	// 1. O checksums.txt assinado, para esta versão.
	checksums, assinatura, err := buscarAssinado(ctx, cliente, bases, op.Chaves, alvo)
	if err != nil {
		return nil, err
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
	baixado, err := baixarPara(ctx, cliente, bases, nome, l.Novo, resumoAssinado)
	if err != nil {
		return nil, err
	}
	feito := false
	defer func() {
		if !feito {
			_ = os.Remove(l.Novo)
		}
	}()
	// Entre o download e aqui o caminho pode ter sido trocado por outra coisa:
	// só um arquivo comum ganha permissão de execução e é executado.
	if err := arquivoComum(l.Novo); err != nil {
		return nil, falha(FalhaPermissoes, "%w", err)
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
	// O resumo do programa que sai: a volta só usa o `.anterior` que confere.
	resumoDoQueSai, _ := resumoDoArquivo(l.Atual)
	novoEstado := Estado{
		Situacao:       SituacaoEmProva,
		Versao:         alvo.String(),
		VersaoAnterior: atual.String(),
		Sha256:         baixado,
		Sha256Anterior: resumoDoQueSai,
		AplicadaEm:     agora().UTC(),
		PeloLancador:   op.PeloLancador,
	}
	desfazerEstado := func() {
		if tinhaEstado {
			_ = l.restaurarEstado(anterior)
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

// autoTestar executa o programa novo. Código de saída diferente de zero
// (self_test_failed), ou programa que nem executa (self_test_blocked: no
// Windows com o Smart App Control ligado, o .exe sem assinatura de código é
// barrado), é falha: o programa atual fica.
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
	var saiu *exec.ExitError
	if err != nil && !errors.As(err, &saiu) && tctx.Err() == nil {
		// Nem chegou a rodar: é desta máquina (Smart App Control, pasta
		// montada com noexec, outra arquitetura), e não do programa novo. O
		// código é outro para o painel não contar como falha da versão.
		return falha(FalhaAutoTesteBarrado, "o programa novo não pôde ser executado nesta máquina: %v: %s", err, fimDaSaida(saida))
	}
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
