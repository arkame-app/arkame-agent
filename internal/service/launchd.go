//go:build darwin

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/segredo"
)

func defaultScope() Scope {
	if os.Geteuid() == 0 {
		return ScopeSystem
	}
	return ScopeUser
}

func installPlatform(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	lbl := LaunchdLabel(opts.Name)

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

	plist := montarPlist(lbl, opts.BinaryPath, cfg.ConfigPath, logPath, opts.Scope)

	// Troca atômica com fsync (segredo.Gravar, 0644): o os.WriteFile truncava a
	// plist no lugar, e um corte de energia no meio a deixava vazia ou pela
	// metade — o serviço não subia no boot seguinte.
	if err := segredo.Gravar(plistPath, []byte(plist), 0o644); err != nil {
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
		StartCmd:  RestartCommand(opts.Name, opts.Scope),
		StatusCmd: fmt.Sprintf("launchctl print %s", target),
		LogsCmd:   fmt.Sprintf("tail -f %s", logPath),
	}, nil
}

// servicosDoAgente: os jobs app.arkame.* do sistema e do usuário, pelo nome
// de serviço que os gerou (app.arkame.agent-aws → arkame-agent-aws). Todo
// plist do agente tem o label app.arkame.*, qualquer que seja o nome dado no
// install: o programa não precisa ser conferido.
func servicosDoAgente(string) []string {
	var nomes []string
	dirs := []string{"/Library/LaunchDaemons"}
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(h, "Library", "LaunchAgents"))
	}
	for _, d := range dirs {
		m, _ := filepath.Glob(filepath.Join(d, "app.arkame.*.plist"))
		for _, f := range m {
			nomes = append(nomes, "arkame-"+strings.TrimPrefix(strings.TrimSuffix(filepath.Base(f), ".plist"), "app.arkame."))
		}
	}
	return nomes
}

// pastasDePlists é onde ficam os plists de cada escopo.
func pastasDePlists() map[Scope]string {
	pastas := map[Scope]string{ScopeSystem: "/Library/LaunchDaemons"}
	if h, err := os.UserHomeDir(); err == nil {
		pastas[ScopeUser] = filepath.Join(h, "Library", "LaunchAgents")
	}
	return pastas
}

// registrados lê os jobs app.arkame.* do sistema e do usuário, com o programa
// de cada um, pelo nome de serviço que os gerou (como servicosDoAgente).
func registrados() []servicoRegistrado {
	var todos []servicoRegistrado
	for _, escopo := range []Scope{ScopeSystem, ScopeUser} {
		dir, ok := pastasDePlists()[escopo]
		if !ok {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(dir, "app.arkame.*.plist"))
		for _, f := range m {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			todos = append(todos, servicoRegistrado{
				nome:     "arkame-" + strings.TrimPrefix(strings.TrimSuffix(filepath.Base(f), ".plist"), "app.arkame."),
				escopo:   escopo,
				programa: programaDoPlist(string(b)),
			})
		}
	}
	return todos
}

// rodandoNoSO: o job está carregado e rodando (launchctl print → state =
// running).
func rodandoNoSO(s servicoRegistrado) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := executar(ctx, "launchctl", "print", serviceTarget(s.escopo, LaunchdLabel(s.nome)))
	return err == nil && launchdRodando(string(out))
}

// servicoChamaPrograma diz se há um plist do serviço nome, do sistema ou do
// usuário, que roda o programa exe.
func servicoChamaPrograma(nome, exe string) bool {
	if exe == "" {
		return false
	}
	dirs := []string{"/Library/LaunchDaemons"}
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(h, "Library", "LaunchAgents"))
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, LaunchdLabel(nome)+".plist"))
		if err == nil && mesmoPrograma("darwin", programaDoPlist(string(b)), exe) {
			return true
		}
	}
	return false
}

// configDoServico lê o --config do plist de um agente no escopo dado.
func configDoServico(nome string, escopo Scope) (string, bool) {
	var dir string
	switch escopo {
	case ScopeSystem:
		dir = "/Library/LaunchDaemons"
	case ScopeUser:
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(h, "Library", "LaunchAgents")
	default:
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, LaunchdLabel(nome)+".plist"))
	if err != nil {
		return "", false
	}
	c := configDoPlist(string(b))
	return c, c != ""
}

// Parar: fora do Windows o programa pode ser trocado com o serviço de pé.
func Parar(string) {}

func restartArgs(name string, scope Scope) []string {
	return []string{"launchctl", "kickstart", "-k", serviceTarget(scope, LaunchdLabel(name))}
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

func uninstallPlatform(ctx context.Context, opts Options) error {
	lbl := LaunchdLabel(opts.Name)

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
