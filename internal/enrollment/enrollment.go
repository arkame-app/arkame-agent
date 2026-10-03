// Package enrollment implementa o fluxo de registro do agent no painel.
//
// Passos:
//  1. Gera keypair Ed25519 local
//  2. Calcula fingerprint (SHA-256 hex do pubkey)
//  3. POST /api/agents/enroll com enrollment_token + pubkey + hostname
//  4. Imprime fingerprint para o usuário comparar visualmente no painel
//  5. Long-poll em WaitURL até painel aprovar e emitir um JWT bearer
//  6. Grava a identidade nova de uma vez (Concluir): chave privada em
//     /etc/arkame/key.pem (0600), agent.id, AGENT_ID no env-file e o token em
//     /etc/arkame/token.jwt (0600)
//
// Até a aprovação nada vai ao disco: numa reinstalação que nunca é aprovada,
// o agente segue com a identidade antiga (AGENT_ID e token casados).
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

// Result é o retorno do fluxo de enrollment: a identidade nova, ainda
// pendente — só em memória até Concluir.
type Result struct {
	AgentID     string
	Fingerprint string
	WaitURL     string

	chave    *crypto.Keypair
	resposta api.EnrollResponse
}

// Run executa o enrollment (não aguarda aprovação humana — retorna logo
// após o painel confirmar recebimento). O caller faz o long-poll com
// WaitForApproval e, aprovado, grava tudo com Concluir.
//
// Run não grava nada: antes, o AGENT_ID novo ia para o agent.id e o env-file
// já aqui, com o token antigo no disco. Uma reinstalação nunca aprovada (ou
// interrompida) deixava o agente com AGENT_ID novo e token velho — 403 em
// toda chamada depois do próximo reinício.
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

	// 2. Calcular fingerprint
	fp := crypto.Fingerprint(kp.Public)

	// 3. Enviar ao painel
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

	if resp.AgentID == "" {
		return nil, errors.New("o painel não devolveu o agent_id do enrollment")
	}

	return &Result{
		AgentID:     resp.AgentID,
		Fingerprint: fp,
		WaitURL:     resp.WaitURL,
		chave:       kp,
		resposta:    resp,
	}, nil
}

// Concluir grava a identidade aprovada de uma vez: a chave privada, o
// agent.id, o AGENT_ID (e o armazenamento) no env-file e o token. As linhas
// pendentes do install (armazenamento e chave testados, caminhos da
// identidade) vão ao env-file junto com o AGENT_ID: antes da aprovação o
// arquivo não muda, e Ctrl-C na espera o deixa como estava. O token
// por último: sem ele, o daemon não sobe, em vez de subir com AGENT_ID e
// token de identidades diferentes. Depois tira do env-file o
// ENROLLMENT_TOKEN, já usado.
func Concluir(cfg *config.Config, r *Result, token string, pendentes []string) error {
	if r == nil || r.chave == nil {
		return errors.New("enrollment sem identidade pendente")
	}
	if err := r.chave.SaveToDisk(cfg.PrivateKeyPath); err != nil {
		return fmt.Errorf("salvando private key em %s: %w", cfg.PrivateKeyPath, err)
	}
	slog.Info("private key gravada", "path", cfg.PrivateKeyPath)
	if err := persistAgentID(cfg, r.AgentID); err != nil {
		return fmt.Errorf("persistindo agent_id: %w", err)
	}
	if err := persistirNoArquivo(cfg, r.resposta, pendentes); err != nil {
		return fmt.Errorf("gravando a identidade nova em %s: %w", cfg.ConfigPath, err)
	}
	if err := PersistToken(cfg, token); err != nil {
		return err
	}
	// O código de instalação vale uma vez e já foi usado. Deixado no arquivo,
	// o `status` mostrava "PENDENTE" para sempre num agente aprovado.
	cfg.EnrollmentToken = ""
	if cfg.ConfigPath != "" {
		if err := setup.Remover(cfg.ConfigPath, "ENROLLMENT_TOKEN"); err != nil {
			slog.Warn("não consegui tirar o ENROLLMENT_TOKEN usado do arquivo", "path", cfg.ConfigPath, "err", err)
		}
	}
	return nil
}

// WaitForApproval faz long-poll na WaitURL até o painel aprovar o agent
// e emitir o JWT bearer. Retorna TokenResponse para o caller persistir.
//
// O backend mantém a conexão aberta até ~30s — se ainda pending, responde 204
// e o agent reabre. Se rejected/archived, responde 410 e o agent aborta.
func WaitForApproval(ctx context.Context, cfg *config.Config, r *Result) (*api.TokenResponse, error) {
	// A chave que provamos possuir é a gerada no enrollment, e o agent_id é
	// o que ele devolveu — os dois ainda só em memória. Sem a chave não há
	// como buscar o token: a rota deixou de aceitar quem só conhece o
	// agent_id.
	if r == nil || r.chave == nil {
		return nil, errors.New("enrollment sem identidade pendente")
	}
	kp, agentID := r.chave, r.AgentID
	// WaitURL pode ser absoluto ou relativo ao painel
	base := cfg.PanelURL
	u, parseErr := url.Parse(r.WaitURL)
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
func persistirNoArquivo(cfg *config.Config, resp api.EnrollResponse, pendentes []string) error {
	if resp.AgentID != "" {
		cfg.AgentID = resp.AgentID
	}
	if cfg.ConfigPath == "" {
		return nil
	}
	var linhas []string
	if resp.AgentID != "" {
		linhas = append(linhas, "AGENT_ID="+resp.AgentID)
	}
	if resp.StorageID != "" {
		linhas = append(linhas, "STORAGE_ID="+resp.StorageID)
		cfg.StorageID = resp.StorageID
	}
	if resp.StorageBucket != "" {
		linhas = append(linhas, "STORAGE_BUCKET="+resp.StorageBucket)
		cfg.StorageBucket = resp.StorageBucket
	}
	// A resposta da aprovação vence a pendente da mesma chave (o AGENT_ID
	// que o install-config adiantou, por exemplo).
	daResposta := map[string]bool{}
	for _, l := range linhas {
		k, _, _ := strings.Cut(l, "=")
		daResposta[k] = true
	}
	var todas []string
	for _, l := range pendentes {
		if k, _, _ := strings.Cut(l, "="); !daResposta[k] {
			todas = append(todas, l)
		}
	}
	linhas = append(todas, linhas...)
	if len(linhas) == 0 {
		return nil
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
