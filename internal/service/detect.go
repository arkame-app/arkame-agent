package service

import (
	"os"
	"runtime"
	"strings"
)

// Detected descreve como este processo do agent está sendo executado, para o
// painel poder mostrar ao usuário o comando EXATO de reinício quando o agent
// cair — sem ele ter que caçar o nome do serviço.
type Detected struct {
	// Name é o nome do serviço/unit (ex.: "arkame-agent-aws"). Vazio se o
	// processo não está sob um serviço reconhecido (rodando à mão).
	Name string `json:"name,omitempty"`
	// Scope: "user" (systemd --user) | "system" (systemd root) | "launchd" |
	// "windows" | "" (desconhecido / execução manual).
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
		// launchd: o label é fixo na instalação padrão.
		if _, err := os.Stat("/Library/LaunchDaemons/app.arkame.agent.plist"); err == nil {
			return Detected{Name: "app.arkame.agent", Scope: "launchd"}
		}
		return Detected{}
	case "windows":
		return Detected{Name: "ArkameAgent", Scope: "windows"}
	default:
		return Detected{}
	}
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
