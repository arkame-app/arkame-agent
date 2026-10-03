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
//
// Sem PrivateTmp: com ele o serviço via um /tmp só dele — o backup de /tmp
// saía vazio, e a restauração para /tmp gravava no /tmp privado, que some ao
// parar o serviço. Efeito conhecido do ProtectSystem=full: restaurar para
// /usr, /boot ou /etc falha com "read-only file system" — falha visível no
// painel (error_code read_only_destination, com a explicação; ver
// restore.CodigoDestinoSomenteLeitura e o README), e não arquivo perdido;
// quem precisar restaura em outra pasta, ou usa o agente em Docker.
const systemUnitTmpl = `[Unit]
Description=Arkame Backup Agent (%[1]s)
Documentation=https://arkame.app/docs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%[2]s run --config %[3]s
EnvironmentFile=%[4]s
Restart=always
RestartSec=10s
User=root
Group=root

# Hardening compatível com a função do agent (ler o disco para backup)
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=%[5]s

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
EnvironmentFile=%[4]s
Restart=always
RestartSec=10s

[Install]
WantedBy=default.target
`

// textoDaUnit monta a unit. Os caminhos passam por argDaUnit: o --config
// cru partia em dois argumentos um caminho com espaço (/srv/arkame cfg/…, ou
// um $XDG_CONFIG_HOME com espaço no escopo user), e o serviço subia com o
// arquivo errado e ficava reiniciando. O EnvironmentFile= não tira aspas (o
// valor inteiro é o caminho): nele, só o % é escapado.
func textoDaUnit(escopo Scope, nome, programa, configPath string, writable []string) string {
	if escopo == ScopeSystem {
		return fmt.Sprintf(systemUnitTmpl, nome, argDaUnit(programa), argDaUnit(configPath),
			escaparPorcento(configPath), strings.Join(writable, " "))
	}
	return fmt.Sprintf(userUnitTmpl, nome, argDaUnit(programa), argDaUnit(configPath),
		escaparPorcento(configPath))
}

// argDaUnit é o quoteArg com o % escapado: na unit, %h, %u… são
// especificadores do systemd, e um % literal se escreve %%.
func argDaUnit(s string) string {
	return escaparPorcento(quoteArg(s))
}

func escaparPorcento(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

func defaultScope() Scope {
	if os.Geteuid() == 0 {
		return ScopeSystem
	}
	return ScopeUser
}

func installPlatform(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	return installSystemd(ctx, cfg, opts)
}

// executar roda um comando e devolve a saída combinada; procurar acha o
// executável no PATH. Variáveis para os testes trocarem o systemctl de verdade.
var (
	executar = func(ctx context.Context, nome string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, nome, args...).CombinedOutput()
	}
	procurar = exec.LookPath
)

func installSystemd(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	if _, err := procurar("systemctl"); err != nil {
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
		unit = textoDaUnit(ScopeSystem, opts.Name, opts.BinaryPath, cfg.ConfigPath, writable)
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
		unit = textoDaUnit(ScopeUser, opts.Name, opts.BinaryPath, cfg.ConfigPath, nil)
		sysctl = []string{"systemctl", "--user"}
	}

	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return nil, fmt.Errorf("escrevendo %s: %w", unitPath, err)
	}
	slog.Info("unit systemd criada", "path", unitPath, "scope", string(opts.Scope))

	run := func(args ...string) error {
		full := append(append([]string{}, sysctl[1:]...), args...)
		out, err := executar(ctx, sysctl[0], full...)
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	if err := run("daemon-reload"); err != nil {
		return nil, err
	}

	if err := run("enable", opts.Name); err != nil {
		return nil, err
	}
	// restart, e não `enable --now`: o --now não mexe numa unit já ativa, e na
	// reinstalação (ou atualização) o processo antigo seguia de pé com o token
	// e o AGENT_ID antigos na memória — e tomava 401 assim que a aprovação
	// revogava os tokens anteriores. restart sobe a unit parada e troca a que
	// está rodando.
	if opts.Start {
		if err := run("restart", opts.Name); err != nil {
			return nil, err
		}
	}

	inst := &Installed{
		Name:     opts.Name,
		Scope:    opts.Scope,
		UnitPath: unitPath,
	}

	if opts.Scope == ScopeUser {
		inst.StartCmd = RestartCommand(opts.Name, opts.Scope)
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
		inst.StartCmd = RestartCommand(opts.Name, opts.Scope)
		inst.StatusCmd = fmt.Sprintf("sudo systemctl status %s", opts.Name)
		inst.LogsCmd = fmt.Sprintf("sudo journalctl -u %s -f", opts.Name)
	}

	return inst, nil
}

// servicosDoAgente: as units do sistema e do usuário que são do agente — as
// arkame-agent* e as que chamam o programa exe com outro nome.
func servicosDoAgente(exe string) []string {
	dirs := []string{"/etc/systemd/system"}
	if d, err := userUnitDir(); err == nil {
		dirs = append(dirs, d)
	}
	return unitsDoAgente(dirs, exe)
}

// unitsDoAgente procura nas pastas de units as do agente.
func unitsDoAgente(dirs []string, exe string) []string {
	var nomes []string
	visto := map[string]bool{}
	for _, d := range dirs {
		m, _ := filepath.Glob(filepath.Join(d, "*.service"))
		for _, f := range m {
			nome := strings.TrimSuffix(filepath.Base(f), ".service")
			if visto[nome] {
				continue
			}
			if strings.HasPrefix(nome, DefaultName) || unitChamaPrograma(f, exe) {
				visto[nome] = true
				nomes = append(nomes, nome)
			}
		}
	}
	return nomes
}

// servicoChamaPrograma diz se há uma unit nome, do sistema ou do usuário,
// que roda o programa exe.
func servicoChamaPrograma(nome, exe string) bool {
	dirs := []string{"/etc/systemd/system"}
	if d, err := userUnitDir(); err == nil {
		dirs = append(dirs, d)
	}
	return unitEmChamaPrograma(dirs, nome, exe)
}

// unitEmChamaPrograma procura a unit nome nas pastas dadas.
func unitEmChamaPrograma(dirs []string, nome, exe string) bool {
	for _, d := range dirs {
		if unitChamaPrograma(filepath.Join(d, nome+".service"), exe) {
			return true
		}
	}
	return false
}

// unitChamaPrograma diz se o ExecStart da unit roda o programa exe.
func unitChamaPrograma(arquivo, exe string) bool {
	if exe == "" {
		return false
	}
	b, err := os.ReadFile(arquivo)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ExecStart="); ok {
			// Prefixos do systemd (-, @, :, +, !) vêm antes do caminho.
			v = strings.ReplaceAll(strings.TrimLeft(v, "-@:+!"), "%%", "%")
			if mesmoPrograma("linux", programaDaLinha(v), exe) {
				return true
			}
		}
	}
	return false
}

// configDoServico lê o env-file da unit de um agente no escopo dado.
func configDoServico(nome string, escopo Scope) (string, bool) {
	var dir string
	switch escopo {
	case ScopeSystem:
		dir = "/etc/systemd/system"
	case ScopeUser:
		d, err := userUnitDir()
		if err != nil {
			return "", false
		}
		dir = d
	default:
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, nome+".service"))
	if err != nil {
		return "", false
	}
	c := configDaUnit(string(b))
	return c, c != ""
}

// Parar: fora do Windows o programa pode ser trocado com o serviço de pé.
func Parar(string) {}

func restartArgs(name string, scope Scope) []string {
	if scope == ScopeUser {
		return []string{"systemctl", "--user", "restart", name}
	}
	return []string{"systemctl", "restart", name}
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
		out = append(out, argDaUnit(d))
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
	if _, err := procurar("loginctl"); err != nil {
		return err
	}
	u := currentUsername()
	if u == "" {
		return fmt.Errorf("usuário atual desconhecido")
	}
	// Já habilitado? Então não precisa de sudo.
	if out, err := executar(ctx, "loginctl", "show-user", u, "--property=Linger"); err == nil {
		if strings.Contains(string(out), "Linger=yes") {
			return nil
		}
	}
	if out, err := executar(ctx, "loginctl", "enable-linger", u); err != nil {
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
