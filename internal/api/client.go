package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client é o HTTP client do agent para o painel Arkame.
//
// Modo de autenticação:
//   - Antes do enrollment completo: sem auth, usa só enrollment_token no body
//   - Depois do enrollment aprovado: bearer JWT no header Authorization
//
// (mTLS está planejado como hardening em fase futura — por enquanto o transport
// é HTTPS padrão com verificação do CA do sistema operacional.)
type Client struct {
	baseURL    string
	httpClient *http.Client
	bearer     string // JWT emitido pelo painel; vazio antes da aprovação
}

// Options configura o client.
type Options struct {
	BaseURL string
	// Bearer token — se non-empty, o client envia "Authorization: Bearer <bearer>"
	// em todos os requests.
	Bearer  string
	Timeout time.Duration
}

// New cria um client. Use Options.Bearer para autenticar requests pós-approval.
func New(opts Options) (*Client, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &Client{
		baseURL:    opts.BaseURL,
		httpClient: &http.Client{Transport: transport, Timeout: opts.Timeout},
		bearer:     opts.Bearer,
	}, nil
}

// POST faz um POST JSON e decodifica a resposta em out (se non-nil).
func (c *Client) POST(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req)
	return c.do(req, out)
}

// GET faz um GET e decodifica em out.
//
// Retorna ErrNotReady se o servidor responder 204 (long-poll sem evento).
// Retorna ErrGone se o servidor responder 410 (rejected/archived).
func (c *Client) GET(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	return c.do(req, out)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "arkame-agent")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
}

// ErrNotReady é retornado pelo GET quando o servidor responde 204 (long-poll sem evento).
var ErrNotReady = fmt.Errorf("not ready (204)")

// ErrGone é retornado quando o servidor responde 410 (recurso encerrado: rejected/archived).
var ErrGone = fmt.Errorf("gone (410)")

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return ErrNotReady
	}
	if resp.StatusCode == http.StatusGone {
		return ErrGone
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
