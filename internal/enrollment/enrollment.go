// Package enrollment implementa o fluxo de registro do agent no painel.
//
// Passos:
//  1. Gera keypair Ed25519 local
//  2. Calcula fingerprint (SHA-256 hex do pubkey)
//  3. POST /api/agents/enroll com enrollment_token + pubkey + hostname
//  4. Persiste private key em /etc/arkame/key.pem (0600)
//  5. Imprime fingerprint para o usuário comparar visualmente no painel
//  6. Long-poll em WaitURL até painel aprovar e emitir um JWT bearer
//  7. Salva token em /etc/arkame/token.jwt (0600)
//
// Para re-enrollment (trocar servidor físico mantendo agent_id), o fluxo
// é o mesmo — o painel identifica pelo enrollment_token se é first-time
// ou reinstall e cuida do lado dele (incrementa enrollment_count, revoga
// token antigo após aprovação da nova fingerprint).
//
// Decisão arquitetural (Sprint 5): JWT bearer no lugar de mTLS na fase 1.
// mTLS volta como hardening de transport em fase futura.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
	"github.com/arkame-app/agent/internal/segredo"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/pkg/version"
)

// Options passadas pelo comando install.
type Options struct {
	Hostname      string
	OS            string // "linux-amd64"
	InstallMethod string // "docker" | "binary"
}

// Result é o retorno do fluxo de enrollment.
type Result struct {
	AgentID     string
	Fingerprint string
	WaitURL     string
}

// Run executa o enrollment (não aguarda aprovação humana — retorna logo
// após o painel confirmar recebimento). O caller deve fazer separadamente
// o long-poll em Result.WaitURL para buscar o cert quando aprovado, ou
// deixar isso para o daemon na primeira execução.
func Run(ctx context.Context, cfg *config.Config, o Options) (*Result, error) {
	if o.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("detectando hostname: %w", err)
		}
		o.Hostname = h
	}

	// 1. Gerar keypair
	kp, err := crypto.Generate()
	if err != nil {
		return nil, err
	}

	// 2. Persistir private key (0600)
	if err := kp.SaveToDisk(cfg.PrivateKeyPath); err != nil {
		return nil, fmt.Errorf("salvando private key em %s: %w", cfg.PrivateKeyPath, err)
	}
	slog.Info("private key gravada", "path", cfg.PrivateKeyPath)

	// 3. Calcular fingerprint
	fp := crypto.Fingerprint(kp.Public)

	// 4. Enviar ao painel
	client, err := api.New(api.Options{
		BaseURL: cfg.PanelURL,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	req := api.EnrollRequest{
		EnrollmentToken: cfg.EnrollmentToken,
		PublicKey:       []byte(kp.Public),
		Fingerprint:     fp,
		Hostname:        o.Hostname,
		OS:              o.OS,
		AgentVersion:    version.Version,
		InstallMethod:   o.InstallMethod,
	}
	var resp api.EnrollResponse
	if err := client.POST(ctx, "/api/agents/enroll", req, &resp); err != nil {
		return nil, fmt.Errorf("POST enroll: %w", err)
	}

	slog.Info("enrollment recebido pelo painel",
		"agent_id", resp.AgentID,
		"status", resp.Status,
		"wait_url", resp.WaitURL,
		"expires_at", resp.ExpiresAt)

	// Persistir agent_id para o daemon usar depois
	if err := persistAgentID(cfg, resp.AgentID); err != nil {
		return nil, fmt.Errorf("persistindo agent_id: %w", err)
	}
	if err := persistirNoArquivo(cfg, resp); err != nil {
		return nil, fmt.Errorf("gravando a identidade nova em %s: %w", cfg.ConfigPath, err)
	}

	return &Result{
		AgentID:     resp.AgentID,
		Fingerprint: fp,
		WaitURL:     resp.WaitURL,
	}, nil
}

// WaitForApproval faz long-poll na WaitURL até o painel aprovar o agent
// e emitir o JWT bearer. Retorna TokenResponse para o caller persistir.
//
// O backend mantém a conexão aberta até ~30s — se ainda pending, responde 204
// e o agent reabre. Se rejected/archived, responde 410 e o agent aborta.
func WaitForApproval(ctx context.Context, cfg *config.Config, waitURL string) (*api.TokenResponse, error) {
	// A chave que provamos possuir é a mesma gerada no enrollment. Sem ela não
	// há como buscar o token: a rota deixou de aceitar quem só conhece o
	// agent_id.
	kp, err := crypto.LoadPrivate(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("lendo chave privada em %s: %w", cfg.PrivateKeyPath, err)
	}
	agentID, err := readAgentID(cfg)
	if err != nil {
		return nil, err
	}
	// waitURL pode ser absoluto ou relativo ao painel
	base := cfg.PanelURL
	u, parseErr := url.Parse(waitURL)
	if parseErr != nil {
		return nil, parseErr
	}
	if u.IsAbs() {
		base = u.Scheme + "://" + u.Host
	}

	client, err := api.New(api.Options{
		BaseURL: base,
		Timeout: 60 * time.Second, // long-poll: backend mantém aberto até ~30s
	})
	if err != nil {
		return nil, err
	}

	for {
		var tok api.TokenResponse
		err := client.GETSigned(ctx, u.Path, kp.Private, agentID, &tok)
		switch {
		case err == nil:
			if tok.AgentToken != "" {
				return &tok, nil
			}
			// resposta inesperada (200 sem token) — reabre
			slog.Debug("long-poll retornou 200 sem token; reabrindo")
		case errors.Is(err, api.ErrNotReady):
			// 204: ainda pending. Reabre imediatamente.
			slog.Debug("long-poll: ainda pending, reabrindo")
		case errors.Is(err, api.ErrGone):
			return nil, fmt.Errorf("agent rejeitado ou arquivado pelo painel")
		case jaEmitido(err):
			// 409 token_already_issued: o token deste enrollment já foi
			// entregue (a outra execução, ou a uma tentativa interrompida) e
			// não sai de novo. Reabrir não muda a resposta — antes o agente
			// ficava nisto para sempre, calado (log em Debug).
			return nil, errors.New("o painel já entregou o token deste código de instalação e não o entrega de novo: " +
				"gere um código novo no painel (Servidores → ⋯ → Reinstalar) e rode a instalação com ele")
		default:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Debug("long-poll retornou erro transitório, retry em 3s", "err", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
}

// jaEmitido diz se o wait-token respondeu 409: o token já foi emitido para
// este enrollment (token_already_issued).
func jaEmitido(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusConflict
}

// persistirNoArquivo grava a identidade que o enrollment devolveu no
// env-file. O AGENT_ID do arquivo vence o agent.id (config.Load); numa
// reinstalação com código novo a chave antiga ainda funcionava, o arquivo não
// era reescrito, e o daemon subia com o AGENT_ID velho, assinando com a chave
// nova: 403 para sempre. Agora o enrollment bem-sucedido sempre reescreve o
// AGENT_ID (e o armazenamento, quando o painel o informa).
func persistirNoArquivo(cfg *config.Config, resp api.EnrollResponse) error {
	if resp.AgentID != "" {
		cfg.AgentID = resp.AgentID
	}
	if cfg.ConfigPath == "" || resp.AgentID == "" {
		return nil
	}
	linhas := []string{"AGENT_ID=" + resp.AgentID}
	if resp.StorageID != "" {
		linhas = append(linhas, "STORAGE_ID="+resp.StorageID)
		cfg.StorageID = resp.StorageID
	}
	if resp.StorageBucket != "" {
		linhas = append(linhas, "STORAGE_BUCKET="+resp.StorageBucket)
		cfg.StorageBucket = resp.StorageBucket
	}
	for _, l := range linhas {
		// Cada valor é uma linha: uma quebra escreveria outra chave no arquivo.
		if strings.ContainsAny(l, "\r\n") {
			return errors.New("o painel respondeu um valor com quebra de linha")
		}
	}
	return setup.Gravar(cfg.ConfigPath, linhas)
}

func persistAgentID(cfg *config.Config, id string) error {
	path := cfg.AgentIDPath
	if path == "" {
		path = "/etc/arkame/agent.id"
	}
	// Não é segredo (0644 fora do Windows), mas no Windows ganha a mesma ACL:
	// quem pudesse reescrevê-lo trocaria a identidade que o agente assina.
	return segredo.Gravar(path, []byte(id), 0o644)
}

// PersistToken salva o JWT bearer no TokenPath, legível só pelo administrador
// (0600; no Windows, Administradores e SYSTEM).
// O caller decide quando invocar (geralmente após WaitForApproval bem-sucedido).
//
// O TokenPath vem resolvido do config (env-file/env ou default), preservado
// mesmo quando o arquivo ainda não existe — honrando instalações rootless.
func PersistToken(cfg *config.Config, token string) error {
	if cfg.TokenPath == "" {
		cfg.TokenPath = "/etc/arkame/token.jwt"
	}
	if err := segredo.Gravar(cfg.TokenPath, []byte(token), 0o600); err != nil {
		return fmt.Errorf("salvando token: %w", err)
	}
	return nil
}

// readAgentID lê o agent.id gravado no enrollment. É o identificador que vai
// no material assinado, e precisa ser exatamente o que o painel conhece.
func readAgentID(cfg *config.Config) (string, error) {
	path := cfg.AgentIDPath
	if path == "" {
		path = "/etc/arkame/agent.id"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("lendo agent.id em %s: %w", path, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("agent.id vazio em %s", path)
	}
	return id, nil
}
