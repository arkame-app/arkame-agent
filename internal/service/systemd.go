//go:build linux

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/config"
)

// Unit de sistema: roda como root, sobe no boot antes de qualquer login.
// O hardening é deliberadamente conservador — o agent precisa LER o disco
// inteiro para fazer backup, então ProtectSystem=full (que só protege /usr,
// /boot e /etc contra escrita) em vez de strict, e ProtectHome desligado,
// senão /home fica invisível justamente para quem deveria protegê-lo.
const systemUnitTmpl = `[Unit]
Description=Arkame Backup Agent (%[1]s)
Documentation=https://arkame.app/docs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%[2]s run --config %[3]s
EnvironmentFile=%[3]s
Restart=always
RestartSec=10s
User=root
Group=root

# Hardening compatível com a função do agent (ler o disco para backup)
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true
ReadWritePaths=%[4]s

[Install]
WantedBy=multi-user.target
`

// Unit de usuário: instalação sem sudo. Só enxerga o que o usuário enxerga.
const userUnitTmpl = `[Unit]
Description=Arkame Backup Agent (%[1]s)
Documentation=https://arkame.app/docs
After=network-online.target

[Service]
Type=simple
ExecStart=%[2]s run --config %[3]s
EnvironmentFile=%[3]s
Restart=always
RestartSec=10s

[Install]
WantedBy=default.target
`

func defaultScope() Scope {
	if os.Geteuid() == 0 {
		return ScopeSystem
	}
	return ScopeUser
}

func installPlatform(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	return installSystemd(ctx, cfg, opts)
}

func installSystemd(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf(
			"systemctl não encontrado: esta máquina não usa systemd. Rode o agent com um supervisor próprio (ex.: `%s run --config %s`) ou use a imagem Docker",
			opts.BinaryPath, cfg.ConfigPath)
	}

	// Diretórios que o agent precisa escrever: onde ficam token, chave e id.
	writable := writablePaths(cfg)

	var (
		unitPath string
		unit     string
		sysctl   []string
	)

	switch opts.Scope {
	case ScopeSystem:
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf(
				"instalar serviço de sistema exige root: repita com sudo, ou use --service-scope=user para instalar só para o seu usuário (sem sudo)")
		}
		unitPath = filepath.Join("/etc/systemd/system", opts.Name+".service")
		unit = fmt.Sprintf(systemUnitTmpl, opts.Name, quoteArg(opts.BinaryPath), cfg.ConfigPath, strings.Join(writable, " "))
		sysctl = []string{"systemctl"}

	case ScopeUser:
		if os.Geteuid() == 0 {
			return nil, fmt.Errorf(
				"--service-scope=user rodando como root instalaria o serviço para o root, não para você: rode sem sudo")
		}
		dir, err := userUnitDir()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("criando %s: %w", dir, err)
		}
		unitPath = filepath.Join(dir, opts.Name+".service")
		unit = fmt.Sprintf(userUnitTmpl, opts.Name, quoteArg(opts.BinaryPath), cfg.ConfigPath)
		sysctl = []string{"systemctl", "--user"}
	}

	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return nil, fmt.Errorf("escrevendo %s: %w", unitPath, err)
	}
	slog.Info("unit systemd criada", "path", unitPath, "scope", string(opts.Scope))

	run := func(args ...string) error {
		full := append(append([]string{}, sysctl[1:]...), args...)
		cmd := exec.CommandContext(ctx, sysctl[0], full...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	if err := run("daemon-reload"); err != nil {
		return nil, err
	}

	enableArgs := []string{"enable", opts.Name}
	if opts.Start {
		enableArgs = append(enableArgs, "--now")
	}
	if err := run(enableArgs...); err != nil {
		return nil, err
	}

	inst := &Installed{
		Name:     opts.Name,
		Scope:    opts.Scope,
		UnitPath: unitPath,
	}

	if opts.Scope == ScopeUser {
		inst.StartCmd = fmt.Sprintf("systemctl --user restart %s", opts.Name)
		inst.StatusCmd = fmt.Sprintf("systemctl --user status %s", opts.Name)
		inst.LogsCmd = fmt.Sprintf("journalctl --user -u %s -f", opts.Name)
		// Sem lingering, o serviço do usuário morre no logout e não voltaria
		// no reboot — exatamente o modo de falha que já nos custou um restore
		// parado por 10 minutos.
		if err := enableLinger(ctx); err != nil {
			inst.LingerNote = fmt.Sprintf(
				"não consegui habilitar lingering automaticamente (%v). Rode: sudo loginctl enable-linger %s — sem isso o agent para quando você deslogar.",
				err, currentUsername())
		}
	} else {
		inst.StartCmd = fmt.Sprintf("sudo systemctl restart %s", opts.Name)
		inst.StatusCmd = fmt.Sprintf("sudo systemctl status %s", opts.Name)
		inst.LogsCmd = fmt.Sprintf("sudo journalctl -u %s -f", opts.Name)
	}

	return inst, nil
}

// writablePaths lista os diretórios que a unit de sistema precisa liberar para
// escrita, derivados dos caminhos configurados (rootless muda todos eles).
func writablePaths(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath} {
		if p == "" {
			continue
		}
		d := filepath.Dir(p)
		if d == "" || d == "/" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, quoteArg(d))
	}
	if len(out) == 0 {
		out = append(out, "/etc/arkame")
	}
	return out
}

func userUnitDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("descobrindo o home do usuário: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func enableLinger(ctx context.Context) error {
	if _, err := exec.LookPath("loginctl"); err != nil {
		return err
	}
	u := currentUsername()
	if u == "" {
		return fmt.Errorf("usuário atual desconhecido")
	}
	// Já habilitado? Então não precisa de sudo.
	if out, err := exec.CommandContext(ctx, "loginctl", "show-user", u, "--property=Linger").Output(); err == nil {
		if strings.Contains(string(out), "Linger=yes") {
			return nil
		}
	}
	cmd := exec.CommandContext(ctx, "loginctl", "enable-linger", u)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func uninstallPlatform(ctx context.Context, opts Options) error {
	var (
		unitPath string
		sysctl   []string
	)

	switch opts.Scope {
	case ScopeSystem:
		if os.Geteuid() != 0 {
			return fmt.Errorf("remover serviço de sistema exige root: repita com sudo")
		}
		unitPath = filepath.Join("/etc/systemd/system", opts.Name+".service")
		sysctl = []string{"systemctl"}
	case ScopeUser:
		dir, err := userUnitDir()
		if err != nil {
			return err
		}
		unitPath = filepath.Join(dir, opts.Name+".service")
		sysctl = []string{"systemctl", "--user"}
	}

	run := func(args ...string) {
		full := append(append([]string{}, sysctl[1:]...), args...)
		// disable/stop falham se a unit já não existe — não é erro pra nós.
		_ = exec.CommandContext(ctx, sysctl[0], full...).Run()
	}

	run("disable", "--now", opts.Name)

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removendo %s: %w", unitPath, err)
	}
	run("daemon-reload")
	slog.Info("serviço removido", "name", opts.Name, "scope", string(opts.Scope))
	return nil
}
