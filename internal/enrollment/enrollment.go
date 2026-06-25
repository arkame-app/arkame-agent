// Package enrollment implementa o fluxo de registro do agent no painel.
//
// Passos:
//   1. Gera keypair Ed25519 local
//   2. Calcula fingerprint (SHA-256 hex do pubkey)
//   3. POST /api/agents/enroll com enrollment_token + pubkey + hostname
//   4. Persiste private key em /etc/arkame/key.pem (0600)
//   5. Imprime fingerprint para o usuário comparar visualmente no painel
//   6. Long-poll em WaitURL até painel aprovar e emitir um JWT bearer
//   7. Salva token em /etc/arkame/token.jwt (0600)
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
	"net/url"
	"os"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
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
	// waitURL pode ser absoluto ou relativo ao painel
	base := cfg.PanelURL
	u, err := url.Parse(waitURL)
	if err != nil {
		return nil, err
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
		err := client.GET(ctx, u.Path, &tok)
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

func persistAgentID(cfg *config.Config, id string) error {
	path := cfg.AgentIDPath
	if path == "" {
		path = "/etc/arkame/agent.id"
	}
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return fmt.Errorf("criando diretório do agent.id: %w", err)
	}
	return os.WriteFile(path, []byte(id), 0o644)
}

// PersistToken salva o JWT bearer no TokenPath com permissão 0600.
// O caller decide quando invocar (geralmente após WaitForApproval bem-sucedido).
//
// O TokenPath vem resolvido do config (env-file/env ou default), preservado
// mesmo quando o arquivo ainda não existe — honrando instalações rootless.
func PersistToken(cfg *config.Config, token string) error {
	if cfg.TokenPath == "" {
		cfg.TokenPath = "/etc/arkame/token.jwt"
	}
	if err := os.MkdirAll(parentDir(cfg.TokenPath), 0o755); err != nil {
		return fmt.Errorf("criando diretório do token: %w", err)
	}
	if err := os.WriteFile(cfg.TokenPath, []byte(token), 0o600); err != nil {
		return fmt.Errorf("salvando token: %w", err)
	}
	return nil
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
