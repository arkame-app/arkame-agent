package atualizacao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
func ClientePadrao() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
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

func enderecoAceito(endereco string) bool {
	u, err := url.Parse(endereco)
	if err != nil || u.Host == "" || u.User != nil {
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

// buscar baixa um arquivo pequeno do release (checksums.txt e a assinatura),
// da primeira base que responder.
func buscar(ctx context.Context, cliente *http.Client, bases []string, nome string) ([]byte, error) {
	var ultimo error
	for _, base := range bases {
		b, err := buscarDe(ctx, cliente, base+"/"+nome)
		if err == nil {
			return b, nil
		}
		ultimo = err
	}
	if ultimo == nil {
		ultimo = fmt.Errorf("sem endereço de onde baixar %s", nome)
	}
	return nil, ultimo
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
// foi gravado. Em qualquer falha — rede cortada no meio, disco cheio — o
// destino sai do disco: um programa pela metade nunca fica para trás.
func baixarPara(ctx context.Context, cliente *http.Client, bases []string, nome, destino string) (string, error) {
	var ultimo error
	for _, base := range bases {
		resumo, err := baixarDe(ctx, cliente, base+"/"+nome, destino)
		if err == nil {
			return resumo, nil
		}
		_ = os.Remove(destino)
		ultimo = err
		if ctx.Err() != nil {
			break
		}
	}
	if ultimo == nil {
		ultimo = fmt.Errorf("sem endereço de onde baixar %s", nome)
	}
	return "", ultimo
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

// criarNovo abre o destino do download. Variável para os testes simularem o
// disco cheio.
var criarNovo = func(caminho string) (arquivoNovo, error) {
	return os.OpenFile(caminho, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
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
