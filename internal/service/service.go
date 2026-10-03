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
	"path/filepath"
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

// ArquivoDeLog é onde o serviço do Windows grava o log: ao lado do arquivo de
// configuração, com o mesmo nome e extensão .log (C:\etc\arkame\agent.log).
// Pela configuração, e não pelo nome do serviço: o SCM chama `run --config …`
// sem o --service-name, e cada agente da máquina tem o seu arquivo.
func ArquivoDeLog(configPath string) string {
	return strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".log"
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

// LaunchdLabel converte o nome do serviço no identificador reverse-DNS que o
// launchd espera: arkame-agent-aws → app.arkame.agent-aws. Fora de build tag
// porque a CLI também monta o comando de reinício.
//
// Aceita também o próprio label: é o que o agente no macOS reporta ao painel,
// e o comando de reinício do painel o devolve em --service-name.
func LaunchdLabel(name string) string {
	if strings.HasPrefix(name, "app.arkame.") {
		return name
	}
	trimmed := strings.TrimPrefix(name, "arkame-")
	if trimmed == "" {
		trimmed = "agent"
	}
	return "app.arkame." + trimmed
}

// RestartArgs é o comando que reinicia o serviço, já com o escopo certo:
// `systemctl --user` para o serviço do usuário, `gui/<uid>` no launchd do
// usuário. Fonte única do reinício: `install` mostra, `set-storage-keys
// --restart` executa. Escopo vazio = o padrão desta plataforma.
func RestartArgs(name string, scope Scope) []string {
	if name == "" {
		name = DefaultName
	}
	if scope == "" {
		scope = defaultScope()
	}
	return restartArgs(name, scope)
}

// RestartCommand é o RestartArgs para mostrar ao operador (com sudo quando o
// serviço é do sistema, fora do Windows).
func RestartCommand(name string, scope Scope) string {
	if scope == "" {
		scope = defaultScope()
	}
	cmd := strings.Join(RestartArgs(name, scope), " ")
	if scope == ScopeSystem && !strings.HasPrefix(cmd, "powershell") {
		return "sudo " + cmd
	}
	return strings.TrimPrefix(cmd, "powershell -NoProfile -Command ")
}

// OutrosAgentes lista os outros serviços do agente nesta máquina (um por
// credencial de storage, com --service-name). Eles usam o mesmo programa:
// desinstalar um não pode apagá-lo.
func OutrosAgentes(name string) []string {
	if name == "" {
		name = DefaultName
	}
	var outros []string
	for _, n := range servicosDoAgente() {
		if n != name {
			outros = append(outros, n)
		}
	}
	return outros
}

// ConfigsDosOutros devolve o arquivo de configuração de cada outro agente
// desta máquina, lido do registro do serviço (unit, plist, SCM). completo é
// false quando algum deles não pôde ser lido — o chamador não sabe, então,
// que caminhos aquele agente usa.
func ConfigsDosOutros(name string) (configs []string, completo bool) {
	completo = true
	for _, n := range OutrosAgentes(name) {
		c, ok := configDoServico(n)
		if !ok || c == "" {
			completo = false
			continue
		}
		configs = append(configs, c)
	}
	return configs, completo
}

// ConfigDoServico devolve o arquivo de configuração que o serviço name usa,
// lido do registro dele (unit, plist, SCM). ok é false quando o serviço não
// existe ou o registro não cita o arquivo.
func ConfigDoServico(name string) (string, bool) {
	return configDoServico(name)
}

// configDaUnit lê o env-file de uma unit do systemd gerada pelo agente
// (EnvironmentFile=, ou o --config do ExecStart).
func configDaUnit(conteudo string) string {
	var doExec string
	for _, l := range strings.Split(conteudo, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "EnvironmentFile="); ok {
			return strings.Trim(strings.TrimPrefix(v, "-"), `"`)
		}
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			doExec = configDosArgs(strings.Fields(v))
		}
	}
	return strings.Trim(doExec, `"`)
}

// configDoPlist lê o argumento depois de --config no ProgramArguments.
func configDoPlist(conteudo string) string {
	var args []string
	resto := conteudo
	for {
		i := strings.Index(resto, "<string>")
		if i < 0 {
			break
		}
		resto = resto[i+len("<string>"):]
		j := strings.Index(resto, "</string>")
		if j < 0 {
			break
		}
		args = append(args, xmlUnescape(resto[:j]))
		resto = resto[j+len("</string>"):]
	}
	return configDosArgs(args)
}

// configDosArgs acha o valor de --config (separado ou com =).
func configDosArgs(args []string) string {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			return v
		}
	}
	return ""
}

func xmlUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&").Replace(s)
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
