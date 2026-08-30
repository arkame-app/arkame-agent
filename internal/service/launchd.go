//go:build darwin

package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/config"
)

// plistTmpl monta o job do launchd. KeepAlive mantém o agent de pé; RunAtLoad
// sobe junto com o sistema (LaunchDaemon) ou com o login (LaunchAgent).
//
// O launchd não tem EnvironmentFile: o env-file é lido pelo próprio agent via
// --config, e o plist só precisa apontar para ele.
const plistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[1]s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[2]s</string>
		<string>run</string>
		<string>--config</string>
		<string>%[3]s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>%[4]s</string>
	<key>StandardErrorPath</key>
	<string>%[4]s</string>
</dict>
</plist>
`

func defaultScope() Scope {
	if os.Geteuid() == 0 {
		return ScopeSystem
	}
	return ScopeUser
}

// label converte o nome do serviço no identificador reverse-DNS que o launchd
// espera: arkame-agent-aws → app.arkame.agent-aws.
func label(name string) string {
	trimmed := strings.TrimPrefix(name, "arkame-")
	if trimmed == "" {
		trimmed = "agent"
	}
	return "app.arkame." + trimmed
}

func installPlatform(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	lbl := label(opts.Name)

	var plistPath, logPath string
	switch opts.Scope {
	case ScopeSystem:
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf(
				"instalar LaunchDaemon exige root: repita com sudo, ou use --service-scope=user para instalar só para o seu usuário")
		}
		plistPath = filepath.Join("/Library/LaunchDaemons", lbl+".plist")
		logPath = filepath.Join("/var/log", opts.Name+".log")

	case ScopeUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("descobrindo o home do usuário: %w", err)
		}
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("criando %s: %w", dir, err)
		}
		plistPath = filepath.Join(dir, lbl+".plist")
		logDir := filepath.Join(home, "Library", "Logs", "Arkame")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return nil, fmt.Errorf("criando %s: %w", logDir, err)
		}
		logPath = filepath.Join(logDir, opts.Name+".log")
	}

	plist := fmt.Sprintf(plistTmpl,
		xmlEscape(lbl), xmlEscape(opts.BinaryPath), xmlEscape(cfg.ConfigPath), xmlEscape(logPath))

	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return nil, fmt.Errorf("escrevendo %s: %w", plistPath, err)
	}
	slog.Info("plist do launchd criado", "path", plistPath, "label", lbl)

	target := serviceTarget(opts.Scope, lbl)

	// bootout do que já existisse: reinstalar em cima de um job carregado
	// falha com "service already loaded". Ignoramos o erro de "não existe".
	_ = exec.CommandContext(ctx, "launchctl", "bootout", target).Run()

	domain := domainTarget(opts.Scope)
	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, strings.TrimSpace(string(out)))
	}
	if opts.Start {
		if out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", target).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("launchctl kickstart %s: %w: %s", target, err, strings.TrimSpace(string(out)))
		}
	}

	return &Installed{
		Name:      lbl,
		Scope:     opts.Scope,
		UnitPath:  plistPath,
		StartCmd:  fmt.Sprintf("launchctl kickstart -k %s", target),
		StatusCmd: fmt.Sprintf("launchctl print %s", target),
		LogsCmd:   fmt.Sprintf("tail -f %s", logPath),
	}, nil
}

// serviceTarget é o alvo de um job específico (system/<label> ou gui/<uid>/<label>).
func serviceTarget(scope Scope, lbl string) string {
	if scope == ScopeSystem {
		return "system/" + lbl
	}
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), lbl)
}

// domainTarget é o domínio onde o job é carregado.
func domainTarget(scope Scope) string {
	if scope == ScopeSystem {
		return "system"
	}
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

func uninstallPlatform(ctx context.Context, opts Options) error {
	lbl := label(opts.Name)

	var plistPath string
	switch opts.Scope {
	case ScopeSystem:
		if os.Geteuid() != 0 {
			return fmt.Errorf("remover LaunchDaemon exige root: repita com sudo")
		}
		plistPath = filepath.Join("/Library/LaunchDaemons", lbl+".plist")
	case ScopeUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("descobrindo o home do usuário: %w", err)
		}
		plistPath = filepath.Join(home, "Library", "LaunchAgents", lbl+".plist")
	}

	// bootout falha se o job não estiver carregado — não é erro pra nós.
	_ = exec.CommandContext(ctx, "launchctl", "bootout", serviceTarget(opts.Scope, lbl)).Run()

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removendo %s: %w", plistPath, err)
	}
	slog.Info("serviço removido", "label", lbl, "scope", string(opts.Scope))
	return nil
}
