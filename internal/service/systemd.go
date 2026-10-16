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
	"sort"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/segredo"
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
//
// ReadWritePaths= leva também a pasta do programa (pastasGravaveis): a
// atualização automática troca o programa ali, e /usr/local/bin está sob o
// ProtectSystem=full.
//
// ReadWriteDirectories= repete o ReadWritePaths=: o systemd anterior ao 231
// (CentOS/RHEL 7 tem o 219, Ubuntu 16.04 o 229) não conhece ReadWritePaths= e
// ignora a linha, mas aplica o ProtectSystem=full — /etc/arkame ficava só
// leitura e o token renovado não era gravado (EROFS), e o agente subia com o
// token vencido depois do reboot. O systemd novo aceita os dois (o antigo
// virou sinônimo) e soma as listas; repetir o mesmo caminho não muda nada.
//
// TimeoutStopSec= é EsperaParada (segundosDeParada): depois do SIGTERM o
// daemon ainda fecha o que estava fazendo (até 3 × finalizacaoGraca), e o
// padrão de 90s mandava SIGKILL no meio da última etapa.
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
TimeoutStopSec=%[6]d
User=root
Group=root

# Hardening compatível com a função do agent (ler o disco para backup)
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=%[5]s
ReadWriteDirectories=%[5]s

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
TimeoutStopSec=%[5]d

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
			escaparPorcento(configPath), strings.Join(writable, " "), segundosDeParada())
	}
	return fmt.Sprintf(userUnitTmpl, nome, argDaUnit(programa), argDaUnit(configPath),
		escaparPorcento(configPath), segundosDeParada())
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

// procurar acha o executável no PATH. Variável para os testes (ver também
// executar, em reiniciar_unix.go).
var procurar = exec.LookPath

// unitDoServico é o texto da unit que o install e o `service refresh` gravam.
// Na de sistema, as pastas liberadas para escrita são as do token, da chave e
// do id, e a do próprio programa (a atualização automática troca o programa
// ali; ver pastasGravaveis).
func unitDoServico(cfg *config.Config, opts Options) string {
	var gravaveis []string
	if opts.Scope == ScopeSystem {
		gravaveis = pastasGravaveis(cfg, opts.BinaryPath)
	}
	texto := textoDaUnit(opts.Scope, opts.Name, opts.BinaryPath, cfg.ConfigPath, gravaveis)
	// O processo da cópia local para o RustFS só depois de os laços
	// terminarem (o que ainda grava no bucket local termina antes). Com o
	// KillMode padrão, o systemd mandava o SIGTERM ao grupo inteiro de uma vez;
	// com mixed, só ao agente, e o resto leva SIGKILL só se sobrar no fim.
	if cfg.LocalStoreProgram != "" {
		texto = strings.Replace(texto, "\nRestart=always\n", "\nRestart=always\nKillMode=mixed\n", 1)
	}
	return texto
}

func installSystemd(ctx context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	if _, err := procurar("systemctl"); err != nil {
		return nil, fmt.Errorf(
			"systemctl não encontrado: esta máquina não usa systemd. Rode o agent com um supervisor próprio (ex.: `%s run --config %s`) ou use a imagem Docker",
			opts.BinaryPath, cfg.ConfigPath)
	}

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
		unit = unitDoServico(cfg, opts)
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
		unit = unitDoServico(cfg, opts)
		sysctl = []string{"systemctl", "--user"}
	}

	// Troca atômica com fsync (segredo.Gravar, 0644): o os.WriteFile truncava a
	// unit no lugar, e um corte de energia no meio a deixava vazia ou pela
	// metade — o serviço não subia no boot seguinte.
	if err := segredo.Gravar(unitPath, []byte(unit), 0o644); err != nil {
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

// regravarPlatform leva a unit de um serviço já instalado ao modelo desta
// versão e recarrega o systemd — sem habilitar nem reiniciar: o serviço parado
// de propósito continua parado, e quem reinicia é quem chamou.
//
// Existe para a última atualização manual de quem instalou antes da
// atualização automática: a unit de sistema antiga não libera a pasta do
// programa (ver pastasGravaveis), e trocar só o programa deixaria o agente sem
// conseguir se atualizar sozinho. O install.sh a chama (`service refresh`)
// depois de trocar o programa.
//
// A unit só é reescrita quando é um modelo do agente sem edição (unitSemEdicao):
// reescrever por cima apagava o que o administrador pôs à mão — um proxy em
// Environment=, um limite de CPU, um caminho a mais em ReadWritePaths=. A unit
// editada fica como está, e o único trecho que precisa mudar — a pasta do
// programa liberada para escrita — vai num drop-in
// (<unit>.service.d/10-arkame-atualizacao.conf), que o systemd soma à unit.
// No escopo do usuário não há ProtectSystem, e a unit editada não precisa de
// nada.
//
// Devolve o arquivo gravado: a unit, o drop-in, ou "" quando não havia o que
// mudar.
func regravarPlatform(ctx context.Context, cfg *config.Config, opts Options) (string, error) {
	var (
		unitPath string
		args     []string
	)
	switch opts.Scope {
	case ScopeSystem:
		if os.Geteuid() != 0 && !regravarSemRoot {
			return "", fmt.Errorf("regravar a unit de um serviço do sistema exige root: repita com sudo")
		}
		unitPath = filepath.Join(dirUnitsDoSistema, opts.Name+".service")
	case ScopeUser:
		dir, err := userUnitDir()
		if err != nil {
			return "", err
		}
		unitPath = filepath.Join(dir, opts.Name+".service")
		args = []string{"--user"}
	}
	atual, err := os.ReadFile(unitPath)
	if err != nil {
		return "", fmt.Errorf("o serviço %s não está instalado (%s): %w", opts.Name, unitPath, err)
	}
	// O programa e a configuração da unit continuam os que ela já tinha: aqui
	// só muda o modelo.
	if p := programaDaUnit(string(atual)); p != "" && p != opts.BinaryPath {
		if err := ConferirPrograma(opts.Scope, p); err != nil {
			return "", err
		}
		opts.BinaryPath = p
	}
	unit := unitDoServico(cfg, opts)
	dropIn := filepath.Join(unitPath+".d", arquivoDoDropIn)
	gravado := ""
	switch {
	case string(atual) == unit:
		// Já é o modelo desta versão.
	case unitSemEdicao(string(atual), unit):
		if err := segredo.Gravar(unitPath, []byte(unit), 0o644); err != nil {
			return "", fmt.Errorf("escrevendo %s: %w", unitPath, err)
		}
		// O drop-in de uma passada anterior (quando a unit estava editada)
		// repete o que o modelo já traz.
		_ = os.Remove(dropIn)
		_ = os.Remove(filepath.Dir(dropIn)) // só sai se ficou vazia
		gravado = unitPath
	default:
		texto := dropInDaPastaDoPrograma(opts.Scope, string(atual), pastasDoPrograma(opts.BinaryPath))
		if texto == "" {
			slog.Info("a unit foi editada à mão e não precisa de mudança: fica como está", "path", unitPath)
			return "", nil
		}
		if anterior, err := os.ReadFile(dropIn); err == nil && string(anterior) == texto {
			return "", nil
		}
		if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
			return "", fmt.Errorf("criando %s: %w", filepath.Dir(dropIn), err)
		}
		if err := segredo.Gravar(dropIn, []byte(texto), 0o644); err != nil {
			return "", fmt.Errorf("escrevendo %s: %w", dropIn, err)
		}
		slog.Info("a unit foi editada à mão e fica como está; a pasta do programa foi liberada num drop-in",
			"unit", unitPath, "drop_in", dropIn)
		gravado = dropIn
	}
	if gravado == "" {
		return "", nil
	}
	args = append(args, "daemon-reload")
	if out, err := executar(ctx, "systemctl", args...); err != nil {
		return "", fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return gravado, nil
}

// regravarSemRoot: os testes regravam uma "unit de sistema" numa pasta
// temporária sem ser root.
var regravarSemRoot = false

// arquivoDoDropIn é o drop-in que o `service refresh` grava ao lado da unit
// editada à mão.
const arquivoDoDropIn = "10-arkame-atualizacao.conf"

// EDropIn diz se o arquivo devolvido pelo Regravar é o drop-in (a unit foi
// preservada) e não a unit.
func EDropIn(arquivo string) bool { return filepath.Base(arquivo) == arquivoDoDropIn }

// diretiva é uma linha Chave=Valor de uma seção da unit.
type diretiva struct{ secao, chave, valor string }

// diretivasDaUnit lê as diretivas de uma unit, na ordem, sem comentários nem
// linhas em branco.
func diretivasDaUnit(texto string) []diretiva {
	var ds []diretiva
	secao := ""
	for _, l := range strings.Split(texto, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			secao = l[1 : len(l)-1]
			continue
		}
		chave, valor, ok := strings.Cut(l, "=")
		if !ok {
			ds = append(ds, diretiva{secao, l, ""})
			continue
		}
		ds = append(ds, diretiva{secao, strings.TrimSpace(chave), strings.TrimSpace(valor)})
	}
	return ds
}

// valoresDeModelo são, por diretiva, os valores fixos que algum modelo do
// agente já gravou (da primeira unit, de maio, à desta versão). As diretivas
// que dependem da instalação (ExecStart, EnvironmentFile, as pastas de
// escrita) são conferidas à parte, em unitSemEdicao.
var valoresDeModelo = map[string][]string{
	"Unit/Documentation":      {"https://arkame.app/docs", "https://docs.arkame.app"},
	"Unit/After":              {"network-online.target"},
	"Unit/Wants":              {"network-online.target"},
	"Service/Type":            {"simple"},
	"Service/Restart":         {"always"},
	"Service/RestartSec":      {"10s", "5s"},
	"Service/User":            {"root"},
	"Service/Group":           {"root"},
	"Service/NoNewPrivileges": {"true"},
	"Service/ProtectSystem":   {"full", "strict"},
	"Service/ProtectHome":     {"read-only"},
	"Service/PrivateTmp":      {"true"},
	"Install/WantedBy":        {"multi-user.target", "default.target"},
}

// unitSemEdicao diz se a unit instalada é um modelo do agente — o desta
// versão ou um anterior — sem nada posto à mão: toda diretiva dela é uma que
// algum modelo grava, com um valor que algum modelo grava. Aí reescrevê-la com
// o modelo novo não apaga decisão de ninguém.
//
// Não é comparação de texto com os modelos antigos: o jeito de escrever os
// caminhos mudou entre versões (aspas, o % escapado), e a unit de quem nunca a
// editou não bateria. O que se confere é o que cada linha diz.
func unitSemEdicao(instalada, modelo string) bool {
	doModelo := map[string][]string{}
	for _, d := range diretivasDaUnit(modelo) {
		doModelo[d.secao+"/"+d.chave] = append(doModelo[d.secao+"/"+d.chave], d.valor)
	}
	pastasDoModelo := map[string]bool{"/var/lib/arkame": true} // a da primeira unit
	for _, chave := range []string{"Service/ReadWritePaths", "Service/ReadWriteDirectories"} {
		for _, v := range doModelo[chave] {
			for _, p := range camposDaLinha(v) {
				pastasDoModelo[p] = true
			}
		}
	}
	execDoModelo := ""
	if v := doModelo["Service/ExecStart"]; len(v) == 1 {
		execDoModelo = strings.Join(camposDaLinha(v[0]), "\x00")
	}
	configDoModelo := configDaUnit(modelo)
	for _, d := range diretivasDaUnit(instalada) {
		chave := d.secao + "/" + d.chave
		switch chave {
		case "Unit/Description":
			if !strings.HasPrefix(d.valor, "Arkame Backup Agent") {
				return false
			}
		case "Service/ExecStart":
			// Mesmo programa, mesmos argumentos — com ou sem aspas.
			if strings.Join(camposDaLinha(d.valor), "\x00") != execDoModelo {
				return false
			}
		case "Service/EnvironmentFile":
			v := strings.ReplaceAll(strings.Trim(strings.TrimPrefix(d.valor, "-"), `"`), "%%", "%")
			// A primeira unit apontava o EnvironmentFile para outro arquivo
			// do agente; qualquer arquivo ao lado da configuração é do modelo.
			if v != configDoModelo && filepath.Dir(v) != filepath.Dir(configDoModelo) {
				return false
			}
		case "Service/TimeoutStopSec":
			if !contem(doModelo[chave], d.valor) {
				return false
			}
		case "Service/ReadWritePaths", "Service/ReadWriteDirectories":
			// Só pastas que o modelo novo também libera: uma a mais é do
			// administrador.
			for _, p := range camposDaLinha(d.valor) {
				if !pastasDoModelo[p] {
					return false
				}
			}
		default:
			if !contem(valoresDeModelo[chave], d.valor) {
				return false
			}
		}
	}
	return true
}

func contem(lista []string, v string) bool {
	for _, x := range lista {
		if x == v {
			return true
		}
	}
	return false
}

// pastasDoPrograma são as pastas que a atualização automática precisa
// gravar: a do caminho registrado e a do caminho real (ver pastasGravaveis).
func pastasDoPrograma(programa string) []string {
	return pastasGravaveis(&config.Config{}, programa)[1:] // sem o /etc/arkame do writablePaths vazio
}

// dropInDaPastaDoPrograma é o texto do drop-in que libera a pasta do programa
// na unit editada à mão, ou "" quando ela não precisa: escopo do usuário (sem
// ProtectSystem), unit sem ProtectSystem, ou pastas que ela já libera.
//
// ReadWritePaths= é uma lista que o systemd soma entre a unit e os drop-ins:
// o que a unit já libera continua valendo. ReadWriteDirectories= repete para
// o systemd anterior ao 231 (ver systemUnitTmpl).
func dropInDaPastaDoPrograma(escopo Scope, instalada string, pastas []string) string {
	if escopo != ScopeSystem || len(pastas) == 0 {
		return ""
	}
	protegida := false
	liberadas := map[string]bool{}
	for _, d := range diretivasDaUnit(instalada) {
		switch d.secao + "/" + d.chave {
		case "Service/ProtectSystem":
			protegida = d.valor != "" && d.valor != "no" && d.valor != "false"
		case "Service/ReadWritePaths", "Service/ReadWriteDirectories":
			for _, p := range camposDaLinha(d.valor) {
				liberadas[p] = true
			}
		}
	}
	if !protegida {
		return ""
	}
	var faltam []string
	for _, p := range pastas {
		if !liberadas[p] && !liberadas[strings.ReplaceAll(p, "%%", "%")] {
			faltam = append(faltam, p)
		}
	}
	if len(faltam) == 0 {
		return ""
	}
	lista := strings.Join(faltam, " ")
	return "# Gravado pelo `arkame-agent service refresh`. A unit ao lado foi editada à\n" +
		"# mão e ficou como está; este arquivo só libera a pasta do programa do agente\n" +
		"# para escrita, que é o que a atualização automática precisa. Pode ser apagado\n" +
		"# se a unit já a liberar (ReadWritePaths=).\n" +
		"[Service]\n" +
		"ReadWritePaths=" + lista + "\n" +
		"ReadWriteDirectories=" + lista + "\n"
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
	return mesmoPrograma("linux", programaDaUnit(string(b)), exe)
}

// pastaDeUnits é onde ficam as units de um escopo.
type pastaDeUnits struct {
	escopo Scope
	dir    string
}

// dirUnitsDoSistema: variável para os testes.
var dirUnitsDoSistema = "/etc/systemd/system"

func pastasDeUnits() []pastaDeUnits {
	pastas := []pastaDeUnits{{ScopeSystem, dirUnitsDoSistema}}
	if d, err := userUnitDir(); err == nil {
		pastas = append(pastas, pastaDeUnits{ScopeUser, d})
	}
	return pastas
}

// registrados lê as units do sistema e do usuário, com o programa de cada uma.
func registrados() []servicoRegistrado {
	var todos []servicoRegistrado
	for _, p := range pastasDeUnits() {
		m, _ := filepath.Glob(filepath.Join(p.dir, "*.service"))
		for _, f := range m {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			todos = append(todos, servicoRegistrado{
				nome:     strings.TrimSuffix(filepath.Base(f), ".service"),
				escopo:   p.escopo,
				programa: programaDaUnit(string(b)),
			})
		}
	}
	return todos
}

// rodandoNoSO: a unit está ativa (systemctl [--user] is-active). Como root,
// o --user não acha o gerenciador do usuário e responde que não.
func rodandoNoSO(s servicoRegistrado) bool {
	args := []string{"is-active", "--quiet", s.nome}
	if s.escopo == ScopeUser {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := executar(ctx, "systemctl", args...)
	return err == nil
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

// variavelDoServico lê a variável das linhas Environment= da unit e dos
// drop-ins dela (<unit>.d/*.conf, em ordem de nome, como o systemd).
func variavelDoServico(nome string, escopo Scope, chave string) string {
	dir := dirUnitsDoSistema
	if escopo == ScopeUser {
		d, err := userUnitDir()
		if err != nil {
			return ""
		}
		dir = d
	}
	unit := filepath.Join(dir, nome+".service")
	arquivos := []string{unit}
	dropIns, _ := filepath.Glob(filepath.Join(unit+".d", "*.conf"))
	sort.Strings(dropIns)
	valor := ""
	for _, a := range append(arquivos, dropIns...) {
		b, err := os.ReadFile(a)
		if err != nil {
			continue
		}
		if v, tem := variavelDaUnit(string(b), chave); tem {
			valor = v
		}
	}
	return valor
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

// pastasGravaveis são as do writablePaths mais a pasta do programa.
//
// A atualização automática baixa o programa novo para a pasta do atual e o põe
// no lugar por rename — e o ProtectSystem=full deixa /usr (e com ele o
// /usr/local/bin) só para leitura. Sem a pasta aqui, o serviço do sistema não
// consegue se atualizar: é por isso que quem instalou antes desta unit precisa
// rodar o instalador mais uma vez (ele regrava a unit; ver `service refresh`).
// O serviço já roda como root; liberar a pasta devolve a ele, só ali, a
// escrita que o root teria sem o ProtectSystem.
//
// Vale a pasta do caminho registrado e a do caminho real (links resolvidos):
// o rename acontece na real.
func pastasGravaveis(cfg *config.Config, programa string) []string {
	out := writablePaths(cfg)
	visto := map[string]bool{}
	for _, p := range out {
		visto[p] = true
	}
	candidatos := []string{programa}
	if real, err := filepath.EvalSymlinks(programa); err == nil {
		candidatos = append(candidatos, real)
	}
	for _, c := range candidatos {
		if c == "" || !filepath.IsAbs(c) {
			continue
		}
		d := filepath.Dir(c)
		if d == "/" {
			continue
		}
		if a := argDaUnit(d); !visto[a] {
			visto[a] = true
			out = append(out, a)
		}
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
