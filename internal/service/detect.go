package service

import (
	"os"
	"runtime"
	"strings"
	"sync/atomic"
)

// nomeDoServicoWindows é preenchido pelo Execute do serviço no Windows.
var nomeDoServicoWindows atomic.Value

// Detected descreve como este processo do agent está sendo executado, para o
// painel poder mostrar ao usuário o comando EXATO de reinício quando o agent
// cair — sem ele ter que caçar o nome do serviço.
type Detected struct {
	// Name é o nome do serviço/unit (ex.: "arkame-agent-aws"). Vazio se o
	// processo não está sob um serviço reconhecido (rodando à mão).
	Name string `json:"name,omitempty"`
	// Scope: "user" (systemd --user) | "system" (systemd root) | "launchd"
	// (LaunchDaemon) | "launchd-user" (LaunchAgent do usuário) | "windows" |
	// "" (desconhecido / execução manual).
	Scope string `json:"scope,omitempty"`
}

// Detect descobre o serviço sob o qual o agent roda. Em Linux usa o cgroup
// (/proc/self/cgroup), que reflete o unit systemd real — funciona tanto para
// instalações via `arkame-agent install` quanto para units criadas à mão.
func Detect() Detected {
	switch runtime.GOOS {
	case "linux":
		return detectSystemd()
	case "darwin":
		return detectLaunchd(os.Getenv, func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		})
	case "windows":
		// O nome que o SCM entregou ao iniciar o serviço. Era a constante
		// "ArkameAgent", que nunca existiu: o painel mostrava
		// `Restart-Service ArkameAgent`, e o comando falhava.
		if n, ok := nomeDoServicoWindows.Load().(string); ok && n != "" {
			return Detected{Name: n, Scope: "windows"}
		}
		return Detected{}
	default:
		return Detected{}
	}
}

// Variáveis que o plist do launchd entrega ao processo: o label do job e o
// escopo da instalação (system/user). O launchd não tem nada como o cgroup do
// systemd para o processo descobrir o próprio job.
const (
	envNomeDoServico   = "ARKAME_SERVICE_NAME"
	envEscopoDoServico = "ARKAME_SERVICE_SCOPE"
)

// detectLaunchd reporta o label e o escopo que o plist passou. Era sempre
// app.arkame.agent (e só se existisse o LaunchDaemon padrão): o agente com
// --service-name (app.arkame.agent-<sufixo>) ou instalado no escopo do
// usuário (~/Library/LaunchAgents) reportava o serviço errado — ou nenhum.
//
// O LaunchAgent do usuário vai como "launchd-user": o reinício dele é
// gui/<uid>/<label>, sem sudo, e não o system/<label> do "launchd".
//
// Plist de instalação antiga, sem as variáveis: a detecção de antes.
func detectLaunchd(getenv func(string) string, existe func(string) bool) Detected {
	if n := getenv(envNomeDoServico); n != "" {
		escopo := "launchd"
		if getenv(envEscopoDoServico) == string(ScopeUser) {
			escopo = "launchd-user"
		}
		return Detected{Name: n, Scope: escopo}
	}
	if existe("/Library/LaunchDaemons/app.arkame.agent.plist") {
		return Detected{Name: "app.arkame.agent", Scope: "launchd"}
	}
	return Detected{}
}

// detectSystemd lê /proc/self/cgroup e extrai o nome do .service. Para serviços
// de usuário o caminho contém "user@<uid>.service" (scope=user); senão system.
func detectSystemd() Detected {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return Detected{}
	}
	content := string(data)

	scope := "system"
	if strings.Contains(content, "user@") || strings.Contains(content, "user.slice") {
		scope = "user"
	}

	// Pega o último segmento que termina em ".service" e não é o wrapper
	// "user@<uid>.service".
	var name string
	for _, line := range strings.Split(content, "\n") {
		for _, seg := range strings.Split(line, "/") {
			if strings.HasSuffix(seg, ".service") && !strings.HasPrefix(seg, "user@") {
				name = strings.TrimSuffix(seg, ".service")
			}
		}
	}
	if name == "" {
		return Detected{Scope: ""} // rodando à mão, sem unit
	}
	return Detected{Name: name, Scope: scope}
}
