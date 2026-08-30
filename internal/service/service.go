// Package service instala o agent como serviço do SO — systemd no Linux,
// launchd no macOS, Service Control Manager no Windows — e descobre sob qual
// serviço o processo atual está rodando (ver detect.go).
//
// Cada plataforma tem seu arquivo build-tagged (systemd.go, launchd.go,
// windows.go) para manter o binário cross-platform.
package service

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/arkame-app/agent/internal/config"
)

// DefaultName é o nome do serviço quando o operador não escolhe outro.
const DefaultName = "arkame-agent"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Scope decide onde o serviço é registrado.
type Scope string

const (
	// ScopeSystem registra para a máquina inteira e sobe no boot, antes de
	// qualquer login. Exige privilégio de administrador.
	ScopeSystem Scope = "system"
	// ScopeUser registra para o usuário atual — instalação sem sudo. No Linux
	// exige lingering habilitado para sobreviver ao logout (ver systemd.go).
	ScopeUser Scope = "user"
)

// Options controla a instalação do serviço.
type Options struct {
	// Name identifica a unit. Um host pode ter mais de um agent (um por
	// conjunto de credenciais de storage), daí ser parametrizável:
	// arkame-agent-aws, arkame-agent-oci, ...
	Name string
	// Scope: system (root) ou user (rootless).
	Scope Scope
	// BinaryPath é o executável que o serviço chama. Vazio = o binário atual.
	BinaryPath string
	// Start sobe o serviço logo após instalar.
	Start bool
}

// Installed descreve o que foi criado, para a CLI poder dizer ao operador
// exatamente qual comando gerencia o serviço dele.
type Installed struct {
	Name        string
	Scope       Scope
	UnitPath    string
	StartCmd    string
	StatusCmd   string
	LogsCmd     string
	LingerNote  string
	NeedsManual string
}

// Install registra o agent como serviço do SO.
func Install(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	if !nameRe.MatchString(opts.Name) {
		return nil, fmt.Errorf(
			"nome de serviço inválido %q: use minúsculas, números, ponto, hífen ou underscore (até 63 caracteres)",
			opts.Name)
	}
	if opts.Scope == "" {
		opts.Scope = defaultScope()
	}
	if opts.Scope != ScopeSystem && opts.Scope != ScopeUser {
		return nil, fmt.Errorf("escopo inválido %q: use %q ou %q", opts.Scope, ScopeSystem, ScopeUser)
	}

	if opts.BinaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("descobrindo o caminho do próprio binário: %w", err)
		}
		opts.BinaryPath = exe
	}

	if cfg.ConfigPath == "" {
		return nil, fmt.Errorf(
			"não sei qual env-file o serviço deve carregar: rode com --config apontando para o arquivo de credenciais")
	}

	return installPlatform(ctx, cfg, opts)
}

// Uninstall remove o serviço do SO. Não apaga token, chave nem env-file — só
// o registro do serviço; reinstalar em cima continua funcionando.
func Uninstall(ctx context.Context, opts Options) error {
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	if !nameRe.MatchString(opts.Name) {
		return fmt.Errorf("nome de serviço inválido %q", opts.Name)
	}
	if opts.Scope == "" {
		opts.Scope = defaultScope()
	}
	return uninstallPlatform(ctx, opts)
}

// quoteArg protege caminhos com espaço nos arquivos de unit/plist/sc.
func quoteArg(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'\\") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
