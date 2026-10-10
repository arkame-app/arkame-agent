//go:build linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// unitDeSistemaAnterior é a unit de sistema que a versão anterior à
// atualização automática gravava: sem a pasta do programa nas linhas de
// escrita.
const unitDeSistemaAnterior = `[Unit]
Description=Arkame Backup Agent (arkame-agent)
Documentation=https://arkame.app/docs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/arkame-agent run --config /etc/arkame/agent.env
EnvironmentFile=/etc/arkame/agent.env
Restart=always
RestartSec=10s
TimeoutStopSec=150
User=root
Group=root

# Hardening compatível com a função do agent (ler o disco para backup)
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=/etc/arkame
ReadWriteDirectories=/etc/arkame

[Install]
WantedBy=multi-user.target
`

// bancadaDeUnits aponta as units do sistema para uma pasta temporária e conta
// as chamadas ao systemctl.
func bancadaDeUnits(t *testing.T, unit string) (unitPath string, chamadas *[]string) {
	t.Helper()
	dir := t.TempDir()
	antesDir, antesRoot, antesExec := dirUnitsDoSistema, regravarSemRoot, executar
	t.Cleanup(func() { dirUnitsDoSistema, regravarSemRoot, executar = antesDir, antesRoot, antesExec })
	dirUnitsDoSistema, regravarSemRoot = dir, true
	var feitas []string
	executar = func(_ context.Context, nome string, args ...string) ([]byte, error) {
		feitas = append(feitas, nome+" "+strings.Join(args, " "))
		return nil, nil
	}
	unitPath = filepath.Join(dir, "arkame-agent.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	return unitPath, &feitas
}

func regravarDeSistema(t *testing.T) string {
	t.Helper()
	arquivo, err := regravarPlatform(context.Background(),
		&config.Config{ConfigPath: "/etc/arkame/agent.env", TokenPath: "/etc/arkame/token.jwt"},
		Options{Name: "arkame-agent", Scope: ScopeSystem, BinaryPath: "/usr/local/bin/arkame-agent"})
	if err != nil {
		t.Fatal(err)
	}
	return arquivo
}

// A unit do modelo anterior, sem edição, é reescrita com o modelo novo.
func TestRegravarReescreveAUnitDoModeloAnterior(t *testing.T) {
	unitPath, chamadas := bancadaDeUnits(t, unitDeSistemaAnterior)
	if arquivo := regravarDeSistema(t); arquivo != unitPath {
		t.Fatalf("arquivo = %q, queria a unit", arquivo)
	}
	nova, _ := os.ReadFile(unitPath)
	if got := linhaDaUnit(string(nova), "ReadWritePaths"); got != "/etc/arkame /usr/local/bin" {
		t.Fatalf("ReadWritePaths=%s", got)
	}
	if _, err := os.Stat(unitPath + ".d"); err == nil {
		t.Fatal("criou drop-in para uma unit sem edição")
	}
	if strings.Join(*chamadas, "|") != "systemctl daemon-reload" {
		t.Fatalf("comandos = %v", *chamadas)
	}
	// De novo: já é o modelo desta versão, e nada acontece.
	*chamadas = nil
	if arquivo := regravarDeSistema(t); arquivo != "" || len(*chamadas) != 0 {
		t.Fatalf("a segunda passada fez algo: %q %v", arquivo, *chamadas)
	}
}

// A unit com edição manual — um proxy, um limite de CPU, uma pasta a mais —
// NÃO é reescrita: fica byte a byte como estava, e a pasta do programa é
// liberada num drop-in.
func TestRegravarPreservaAUnitEditadaAMao(t *testing.T) {
	edicoes := map[string]string{
		"variável de ambiente": strings.Replace(unitDeSistemaAnterior, "Restart=always\n", "Restart=always\nEnvironment=HTTPS_PROXY=http://proxy.interno:3128\n", 1),
		"limite de CPU":        strings.Replace(unitDeSistemaAnterior, "User=root\n", "CPUQuota=50%\nUser=root\n", 1),
		"pasta a mais":         strings.Replace(unitDeSistemaAnterior, "ReadWritePaths=/etc/arkame\n", "ReadWritePaths=/etc/arkame /mnt/restauracoes\n", 1),
		"valor trocado":        strings.Replace(unitDeSistemaAnterior, "RestartSec=10s", "RestartSec=60s", 1),
		"argumento a mais":     strings.Replace(unitDeSistemaAnterior, "--config /etc/arkame/agent.env\n", "--config /etc/arkame/agent.env --host-root /mnt\n", 1),
		"espera de parada":     strings.Replace(unitDeSistemaAnterior, "TimeoutStopSec=150", "TimeoutStopSec=900", 1),
	}
	for nome, editada := range edicoes {
		t.Run(nome, func(t *testing.T) {
			if editada == unitDeSistemaAnterior {
				t.Fatal("o teste não editou a unit")
			}
			unitPath, chamadas := bancadaDeUnits(t, editada)
			dropIn := filepath.Join(unitPath+".d", arquivoDoDropIn)
			arquivo := regravarDeSistema(t)
			if depois, _ := os.ReadFile(unitPath); string(depois) != editada {
				t.Fatalf("a unit editada à mão foi reescrita:\n%s", depois)
			}
			if arquivo != dropIn || !EDropIn(arquivo) {
				t.Fatalf("arquivo = %q, queria o drop-in %q", arquivo, dropIn)
			}
			texto, err := os.ReadFile(dropIn)
			if err != nil {
				t.Fatal(err)
			}
			ds := diretivasDaUnit(string(texto))
			if len(ds) != 2 || ds[0] != (diretiva{"Service", "ReadWritePaths", "/usr/local/bin"}) ||
				ds[1] != (diretiva{"Service", "ReadWriteDirectories", "/usr/local/bin"}) {
				t.Fatalf("o drop-in traz mais (ou menos) que a pasta do programa:\n%s", texto)
			}
			if strings.Join(*chamadas, "|") != "systemctl daemon-reload" {
				t.Fatalf("comandos = %v", *chamadas)
			}
			// De novo: o drop-in já está lá, e nada é regravado nem recarregado.
			*chamadas = nil
			if arquivo := regravarDeSistema(t); arquivo != "" || len(*chamadas) != 0 {
				t.Fatalf("a segunda passada fez algo: %q %v", arquivo, *chamadas)
			}
		})
	}
}

// A unit editada que já libera a pasta do programa (ou que tirou o
// ProtectSystem) não precisa de drop-in.
func TestRegravarNaoGravaDropInDesnecessario(t *testing.T) {
	for nome, editada := range map[string]string{
		"já libera a pasta": strings.Replace(unitDeSistemaAnterior, "ReadWritePaths=/etc/arkame\n", "ReadWritePaths=/etc/arkame /usr/local/bin /mnt/x\n", 1),
		"sem ProtectSystem": strings.Replace(unitDeSistemaAnterior, "ProtectSystem=full\n", "Nice=10\n", 1),
	} {
		t.Run(nome, func(t *testing.T) {
			unitPath, chamadas := bancadaDeUnits(t, editada)
			if arquivo := regravarDeSistema(t); arquivo != "" {
				t.Fatalf("arquivo = %q", arquivo)
			}
			if depois, _ := os.ReadFile(unitPath); string(depois) != editada {
				t.Fatal("a unit editada à mão foi reescrita")
			}
			if _, err := os.Stat(unitPath + ".d"); err == nil {
				t.Fatal("gravou drop-in sem precisar")
			}
			if len(*chamadas) != 0 {
				t.Fatalf("comandos = %v", *chamadas)
			}
		})
	}
}

// Desfeita a edição, a passada seguinte reescreve a unit e tira o drop-in que
// ela mesma tinha gravado.
func TestRegravarTiraODropInQuandoReescreve(t *testing.T) {
	editada := strings.Replace(unitDeSistemaAnterior, "User=root\n", "CPUQuota=50%\nUser=root\n", 1)
	unitPath, _ := bancadaDeUnits(t, editada)
	dropIn := filepath.Join(unitPath+".d", arquivoDoDropIn)
	if arquivo := regravarDeSistema(t); arquivo != dropIn {
		t.Fatalf("arquivo = %q", arquivo)
	}
	if err := os.WriteFile(unitPath, []byte(unitDeSistemaAnterior), 0o644); err != nil {
		t.Fatal(err)
	}
	if arquivo := regravarDeSistema(t); arquivo != unitPath {
		t.Fatalf("arquivo = %q", arquivo)
	}
	if _, err := os.Stat(dropIn); err == nil {
		t.Fatal("o drop-in ficou ao lado da unit reescrita")
	}
}

// No escopo do usuário não há ProtectSystem: a unit editada fica como está, e
// não há o que gravar.
func TestRegravarPreservaAUnitDoUsuarioEditada(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("o teste usa o serviço do usuário")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dirUnits := filepath.Join(xdg, "systemd", "user")
	if err := os.MkdirAll(dirUnits, 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dirUnits, "arkame-agent.service")
	editada := "[Service]\nExecStart=/opt/arkame/bin/arkame-agent run --config /srv/a.env\nEnvironmentFile=/srv/a.env\nEnvironment=HTTPS_PROXY=http://proxy:3128\nRestart=always\n"
	if err := os.WriteFile(unitPath, []byte(editada), 0o644); err != nil {
		t.Fatal(err)
	}
	antes := executar
	t.Cleanup(func() { executar = antes })
	executar = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("chamou o systemctl sem ter mudado nada")
		return nil, nil
	}
	arquivo, err := Regravar(context.Background(), &config.Config{ConfigPath: "/srv/a.env"}, Options{Name: "arkame-agent", Scope: ScopeUser})
	if err != nil || arquivo != "" {
		t.Fatalf("arquivo=%q err=%v", arquivo, err)
	}
	if depois, _ := os.ReadFile(unitPath); string(depois) != editada {
		t.Fatalf("a unit do usuário editada foi reescrita:\n%s", depois)
	}
}

// As units que os modelos anteriores gravaram, sem edição, são reconhecidas —
// inclusive com os caminhos entre aspas, como as versões mais novas escrevem.
func TestUnitSemEdicao(t *testing.T) {
	cfg := &config.Config{ConfigPath: "/srv/arkame cfg/agent.env", TokenPath: "/srv/arkame cfg/token.jwt"}
	opts := Options{Name: "arkame-agent", Scope: ScopeSystem, BinaryPath: "/usr/local/bin/arkame-agent"}
	modelo := unitDoServico(cfg, opts)
	if !unitSemEdicao(modelo, modelo) {
		t.Fatal("o modelo desta versão foi lido como editado")
	}
	// Com PrivateTmp, da unit de agosto.
	antiga := strings.Replace(modelo, "ProtectSystem=full\n", "ProtectSystem=full\nPrivateTmp=true\n", 1)
	antiga = strings.Replace(antiga, " /usr/local/bin\n", "\n", 2)
	if antiga == modelo || !unitSemEdicao(antiga, modelo) {
		t.Fatalf("a unit do modelo antigo foi lida como editada:\n%s", antiga)
	}
	if unitSemEdicao(strings.Replace(modelo, "[Service]\n", "[Service]\nMemoryMax=2G\n", 1), modelo) {
		t.Fatal("uma diretiva que nenhum modelo grava passou por modelo")
	}
}
