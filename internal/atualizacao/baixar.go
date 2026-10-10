package atualizacao

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ReleasesNoGitHub é a reserva do download: o release de onde o install.sh e
// o setup do Windows já baixam.
const ReleasesNoGitHub = "https://github.com/arkame-app/arkame-agent/releases/download"

// Tetos de tamanho. O programa tem ~15 MB; o teto existe para um endereço
// errado (ou malicioso) não encher o disco do cliente.
const (
	tamanhoMaximoDoPrograma  = 200 << 20
	tamanhoMaximoDoChecksums = 1 << 20
)

// ClientePadrao baixa com o proxy do ambiente (HTTPS_PROXY/NO_PROXY), como o
// client do painel. Sem prazo total: quem limita é o contexto da troca.
//
// Cada redirecionamento passa pela mesma regra do endereço de partida
// (conferirSalto): o release do GitHub responde com um 302 para o
// armazenamento dele, e sem a conferência um endereço https podia mandar o
// download para um http de fora — ou para um serviço da rede do cliente.
func ClientePadrao() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: conferirSalto,
	}
}

// maximoDeSaltos é quantos redirecionamentos um download segue. O GitHub usa
// um; o teto é folga para um espelho, não para um laço.
const maximoDeSaltos = 5

// conferirSalto é o CheckRedirect do ClientePadrao: o alvo de cada salto tem
// de ser https (ou http da própria máquina), sem usuário e senha — a regra do
// endereço de partida. A consulta é aceita aqui: o endereço assinado do
// armazenamento do GitHub é uma.
func conferirSalto(req *http.Request, via []*http.Request) error {
	if len(via) >= maximoDeSaltos {
		return fmt.Errorf("redirecionamentos demais (%d) baixando a atualização", len(via))
	}
	if !origemAceita(req.URL) {
		return fmt.Errorf("redirecionamento recusado: %s://%s não é https", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// basesDaVersao devolve de onde baixar os arquivos de uma versão, na ordem: o
// endereço que o painel mandou e a reserva. Só https — http, só para a própria
// máquina (ensaio e testes). O endereço não é o que dá confiança ao arquivo
// (isso é a assinatura), mas um http em rede alheia entregaria a quem estiver
// no caminho a versão que este servidor vai rodar.
func basesDaVersao(baseDoPainel, reserva string, versao Versao) []string {
	var bases []string
	for _, b := range []string{baseDoPainel, reserva} {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b == "" || !enderecoAceito(b) {
			continue
		}
		bases = append(bases, b+"/v"+versao.String())
	}
	return bases
}

// enderecoAceito vale para a base (a do painel e a reserva). Além da origem,
// recusa consulta e fragmento: o caminho /v<versão>/<arquivo> é colado no fim
// da base, e numa base "https://x/y?z=" ele viraria valor do parâmetro — o
// pedido iria a um endereço que ninguém escolheu.
func enderecoAceito(endereco string) bool {
	u, err := url.Parse(endereco)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(endereco, "?#") {
		return false
	}
	return origemAceita(u)
}

// origemAceita: https, ou http só para a própria máquina (ensaio e testes), e
// sem usuário e senha no endereço.
func origemAceita(u *url.URL) bool {
	if u == nil || u.Host == "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// buscarAssinado baixa o checksums.txt e a assinatura dele da MESMA base e
// confere a assinatura; a primeira base em que o par confere vale. Os dois são
// uma unidade: buscados cada um "da primeira base que responder", uma base que
// respondesse 200 com lixo (página de erro de um proxy, espelho desatualizado)
// reprovava a troca sem a reserva, boa, ser tentada.
//
// O erro, quando nenhuma base serve, é de assinatura se alguma base entregou
// um par que não confere — é o mais grave, e é o que o painel precisa ver.
func buscarAssinado(ctx context.Context, cliente *http.Client, bases []string, chaves []ed25519.PublicKey, versao Versao) (checksums, assinatura []byte, err error) {
	var naoConfere, naoBaixou error
	for _, base := range bases {
		c, err := buscarDe(ctx, cliente, base+"/"+ArquivoDeChecksums)
		if err != nil {
			naoBaixou = fmt.Errorf("baixando o %s: %w", ArquivoDeChecksums, err)
			continue
		}
		a, err := buscarDe(ctx, cliente, base+"/"+ArquivoDaAssinatura)
		if err != nil {
			naoBaixou = fmt.Errorf("baixando o %s: %w", ArquivoDaAssinatura, err)
			continue
		}
		if err := VerificarAssinatura(chaves, versao, c, a); err != nil {
			naoConfere = err
			continue
		}
		return c, a, nil
	}
	switch {
	case naoConfere != nil:
		return nil, nil, &Falha{Codigo: FalhaAssinatura, Err: naoConfere}
	case naoBaixou != nil:
		return nil, nil, falha(FalhaDownload, "%w", naoBaixou)
	}
	return nil, nil, falha(FalhaDownload, "sem endereço de onde baixar o %s", ArquivoDeChecksums)
}

func buscarDe(ctx context.Context, cliente *http.Client, endereco string) ([]byte, error) {
	resp, err := abrir(ctx, cliente, endereco)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, tamanhoMaximoDoChecksums+1))
	if err != nil {
		return nil, fmt.Errorf("lendo %s: %w", endereco, err)
	}
	if len(b) > tamanhoMaximoDoChecksums {
		return nil, fmt.Errorf("%s é maior que o esperado", endereco)
	}
	return b, nil
}

func abrir(ctx context.Context, cliente *http.Client, endereco string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endereco, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "arkame-agent")
	resp, err := cliente.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s respondeu %s", endereco, resp.Status)
	}
	return resp, nil
}

// baixarPara grava o arquivo do release em destino e devolve o SHA-256 do que
// foi gravado, que é o esperado. Em qualquer falha — rede cortada no meio,
// disco cheio — o destino sai do disco: um programa pela metade nunca fica
// para trás.
//
// A base que entrega um arquivo com outro resumo não encerra a busca: a
// seguinte é tentada, como no checksums.txt. Quando nenhuma serve, o erro é o
// do resumo (checksum_mismatch) se alguma entregou o arquivo errado.
func baixarPara(ctx context.Context, cliente *http.Client, bases []string, nome, destino, esperado string) (string, error) {
	var naoConfere, naoBaixou error
	for _, base := range bases {
		resumo, err := baixarDe(ctx, cliente, base+"/"+nome, destino)
		if err == nil && resumo == esperado {
			return resumo, nil
		}
		_ = os.Remove(destino)
		if err == nil {
			naoConfere = fmt.Errorf("o arquivo baixado de %s tem o SHA-256 %s, e o checksums.txt assinado diz %s", base, resumo, esperado)
			continue
		}
		naoBaixou = err
		if ctx.Err() != nil {
			break
		}
	}
	switch {
	case naoConfere != nil:
		return "", &Falha{Codigo: FalhaResumoDoArquivo, Err: naoConfere}
	case naoBaixou != nil:
		return "", falha(FalhaDownload, "%w", naoBaixou)
	}
	return "", falha(FalhaDownload, "sem endereço de onde baixar %s", nome)
}

func baixarDe(ctx context.Context, cliente *http.Client, endereco, destino string) (string, error) {
	resp, err := abrir(ctx, cliente, endereco)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > tamanhoMaximoDoPrograma {
		return "", fmt.Errorf("%s tem %d bytes, mais que o teto", endereco, resp.ContentLength)
	}
	f, err := criarNovo(destino)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, tamanhoMaximoDoPrograma+1))
	if err == nil && n > tamanhoMaximoDoPrograma {
		err = fmt.Errorf("%s é maior que o teto", endereco)
	}
	// Corpo menor que o Content-Length anunciado: o io.Copy já volta
	// "unexpected EOF". Sem Content-Length, quem acusa o corte é o SHA-256.
	if err == nil {
		// No disco antes de entrar no lugar: um corte de energia depois do
		// rename não pode deixar o programa com 0 bytes.
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("baixando %s: %w", endereco, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// arquivoNovo é o que o download precisa do destino: gravar, levar ao disco e
// fechar.
type arquivoNovo interface {
	io.Writer
	Sync() error
	Close() error
}

// criarNovo abre o destino do download. O que estiver no caminho sai antes, e
// a abertura é exclusiva e não segue link: com O_TRUNC, um `.novo` deixado ali
// como link simbólico era seguido — o download sobrescrevia o alvo do link e,
// na troca, o link ia para o lugar do programa. Variável para os testes
// simularem o disco cheio.
var criarNovo = func(caminho string) (arquivoNovo, error) {
	if err := os.Remove(caminho); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("tirando o que estava em %s: %w", caminho, err)
	}
	return os.OpenFile(caminho, os.O_CREATE|os.O_EXCL|os.O_WRONLY|semSeguirLink, 0o700)
}

// arquivoComum recusa o que não é arquivo regular (link, pasta, dispositivo):
// é o que se confere, sem seguir link, antes de dar permissão de execução a um
// arquivo da pasta do programa, de executá-lo ou de pô-lo no lugar.
func arquivoComum(caminho string) error {
	fi, err := os.Lstat(caminho)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s não é um arquivo comum (%s)", caminho, fi.Mode().Type())
	}
	return nil
}

// resumoDoArquivo é o SHA-256 (hex) de um arquivo do disco.
func resumoDoArquivo(caminho string) (string, error) {
	f, err := os.Open(caminho)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
