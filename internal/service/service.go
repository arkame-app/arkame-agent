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
	"runtime"
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

// EscopoEfetivo é o escopo que a instalação usa: o pedido, ou o padrão da
// plataforma (system como root, senão user; no Windows, sempre system).
func EscopoEfetivo(s Scope) Scope {
	if s == "" {
		return defaultScope()
	}
	return s
}

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
	// Programa é o executável que o serviço chama. No macOS é a ele que se
	// dá o Acesso Total ao Disco.
	Programa string
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

// ValidarNome confere o nome de um serviço. Além da forma aceita pelo SO,
// exige o prefixo arkame-agent de um serviço novo: é por ele que o uninstall
// reconhece os outros agentes da máquina — e um agente que ele não reconhece
// perde o programa que divide com os outros.
//
// O prefixo não vale para o serviço que já existe com esse nome e chama este
// mesmo programa: até a v0.4.3 o install aceitava qualquer nome (backup-oci),
// e `service install --name backup-oci` para reinstalar em cima era recusado.
// Esse serviço o uninstall já reconhece pelo programa (servicosDoAgente).
func ValidarNome(name string) error {
	return validarNome(name, func(n string) bool {
		return servicoChamaPrograma(n, ProgramaAtual())
	})
}

// validarNome é o ValidarNome com a consulta ao SO trocável: existente diz se
// já há um serviço com o nome que chama este programa.
func validarNome(name string, existente func(string) bool) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf(
			"nome de serviço inválido %q: use minúsculas, números, ponto, hífen ou underscore (até 63 caracteres)",
			name)
	}
	if !strings.HasPrefix(name, DefaultName) && !existente(name) {
		return fmt.Errorf(
			"nome de serviço %q não começa com %q: use, por exemplo, %s-%s (é por esse prefixo que o agente reconhece os outros agentes desta máquina)",
			name, DefaultName, DefaultName, strings.TrimPrefix(name, "arkame-"))
	}
	return nil
}

// OutrosAgentes lista os outros serviços do agente nesta máquina (um por
// credencial de storage, com --service-name). Eles usam o mesmo programa:
// desinstalar um não pode apagá-lo.
//
// Conta tanto os serviços com o prefixo arkame-agent quanto os que chamam
// este mesmo programa com outro nome: até a v0.4.3 o install aceitava
// qualquer nome (backup-oci), e o uninstall de outro agente apagava o
// programa de que esse serviço depende.
func OutrosAgentes(name string) []string {
	return outrosEntre(runtime.GOOS, servicosDoAgente(ProgramaAtual()), name)
}

// ProgramaAtual é o executável deste processo, com links resolvidos, como
// era quando o processo subiu. Vazio quando o SO não diz. Vai ao painel no
// heartbeat (program_path).
//
// Lido uma vez, no arranque: no Linux o os.Executable lê /proc/self/exe a
// cada chamada, e o nome segue o arquivo. Depois de uma troca por
// renomeia-e-renomeia (sistema de arquivos sem link) feita pelo agente
// vizinho, o processo que ainda roda o programa antigo passava a responder
// "…/arkame-agent.anterior": o program_path ia errado ao painel, e quem
// perguntasse "quais serviços usam este programa" deixava de achar os outros.
func ProgramaAtual() string { return programaNoArranque }

var programaNoArranque = lerProgramaAtual()

func lerProgramaAtual() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

// programaDaLinha devolve o executável de uma linha de comando registrada
// (ExecStart do systemd, BinaryPathName do SCM): o primeiro argumento, com
// ou sem aspas.
func programaDaLinha(linha string) string {
	linha = strings.TrimSpace(linha)
	if strings.HasPrefix(linha, `"`) {
		if i := strings.Index(linha[1:], `"`); i >= 0 {
			return linha[1 : 1+i]
		}
		return linha[1:]
	}
	if i := strings.IndexAny(linha, " \t"); i >= 0 {
		return linha[:i]
	}
	return linha
}

// mesmoPrograma diz se o caminho registrado num serviço é o programa exe
// (já resolvido). No Windows o caminho não distingue maiúsculas.
func mesmoPrograma(goos, registrado, exe string) bool {
	if registrado == "" || exe == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(registrado); err == nil {
		registrado = r
	}
	if goos == "windows" {
		return strings.EqualFold(registrado, exe)
	}
	return filepath.Clean(registrado) == filepath.Clean(exe)
}

// outrosEntre tira name da lista de serviços do agente. A comparação é pela
// identidade do serviço no SO, e não pelo texto: no macOS o painel devolve o
// label (app.arkame.agent-aws) em --service-name, e a lista vem pelo nome
// (arkame-agent-aws) — o agente se contava como "outro", e o uninstall
// deixava a configuração com a chave, o token e a chave privada no disco.
func outrosEntre(goos string, todos []string, name string) []string {
	if name == "" {
		name = DefaultName
	}
	proprio := chaveDoServico(goos, name)
	var outros []string
	for _, n := range todos {
		if chaveDoServico(goos, n) != proprio {
			outros = append(outros, n)
		}
	}
	return outros
}

// chaveDoServico é como o SO identifica o serviço: o label no launchd, o nome
// sem distinção de maiúsculas no SCM do Windows, o nome da unit no systemd.
func chaveDoServico(goos, name string) string {
	switch goos {
	case "darwin":
		return LaunchdLabel(name)
	case "windows":
		return strings.ToLower(name)
	}
	return name
}

// ConfigsDosOutros devolve o arquivo de configuração de cada outro agente
// desta máquina, lido do registro do serviço (unit, plist, SCM). completo é
// false quando algum deles não pôde ser lido — o chamador não sabe, então,
// que caminhos aquele agente usa.
func ConfigsDosOutros(name string) (configs []string, completo bool) {
	completo = true
	for _, n := range OutrosAgentes(name) {
		c, ok := ConfigDoServico(n, "")
		if !ok || c == "" {
			completo = false
			continue
		}
		configs = append(configs, c)
	}
	return configs, completo
}

// ConfigDoServico devolve o arquivo de configuração que o serviço name usa,
// lido do registro dele (unit, plist, SCM) no escopo pedido. Escopo vazio:
// o padrão desta plataforma primeiro, depois o outro. ok é false quando o
// serviço não existe ou o registro não cita o arquivo.
func ConfigDoServico(name string, scope Scope) (string, bool) {
	escopos := []Scope{scope}
	if scope == "" {
		escopos = []Scope{defaultScope(), ScopeSystem, ScopeUser}
	}
	for _, e := range escopos {
		if c, ok := configDoServico(name, e); ok && c != "" {
			return c, true
		}
	}
	return "", false
}

// VariavelDoServico devolve o valor que a definição do serviço dá a uma
// variável de ambiente FORA do arquivo de configuração — o que o administrador
// pôs à mão na unit. Quem lê só o arquivo de configuração (o `uninstall`) não
// a enxerga, e o serviço roda com ela. Vazio quando não há.
//
// Só o systemd é lido (Environment= da unit e dos drop-ins). No Windows e no
// macOS devolve sempre vazio: lá a definição do serviço que o agente grava
// não leva variáveis do administrador.
func VariavelDoServico(name string, scope Scope, chave string) string {
	if name == "" {
		name = DefaultName
	}
	if !nameRe.MatchString(name) {
		return ""
	}
	return variavelDoServico(name, EscopoEfetivo(scope), chave)
}

// variavelDaUnit acha chave nas linhas Environment= de uma unit do systemd
// (ou de um drop-in). Vale a última atribuição; `Environment=` vazio apaga as
// anteriores, como no systemd.
func variavelDaUnit(conteudo, chave string) (valor string, tem bool) {
	for _, l := range strings.Split(conteudo, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(l), "Environment=")
		if !ok {
			continue
		}
		if strings.TrimSpace(v) == "" {
			valor, tem = "", true
			continue
		}
		for _, campo := range camposDaLinha(v) {
			if k, x, ok := strings.Cut(campo, "="); ok && k == chave {
				valor, tem = strings.ReplaceAll(x, "%%", "%"), true
			}
		}
	}
	return valor, tem
}

// configDaUnit lê o env-file de uma unit do systemd gerada pelo agente
// (EnvironmentFile=, ou o --config do ExecStart). O %% da unit é um %.
func configDaUnit(conteudo string) string {
	var doExec string
	for _, l := range strings.Split(conteudo, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "EnvironmentFile="); ok {
			return strings.ReplaceAll(strings.Trim(strings.TrimPrefix(v, "-"), `"`), "%%", "%")
		}
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			doExec = configDosArgs(camposDaLinha(v))
		}
	}
	return strings.ReplaceAll(doExec, "%%", "%")
}

// camposDaLinha parte uma linha de comando da unit em argumentos, com o
// argumento entre aspas (o que o quoteArg gera) inteiro e sem as aspas.
func camposDaLinha(linha string) []string {
	var (
		campos   []string
		atual    strings.Builder
		aspas    bool
		temCampo bool
	)
	for i := 0; i < len(linha); i++ {
		c := linha[i]
		switch {
		case c == '\\' && aspas && i+1 < len(linha) && linha[i+1] == '"':
			atual.WriteByte('"')
			i++
		case c == '"':
			aspas = !aspas
			temCampo = true
		case (c == ' ' || c == '\t') && !aspas:
			if temCampo {
				campos = append(campos, atual.String())
				atual.Reset()
				temCampo = false
			}
		default:
			atual.WriteByte(c)
			temCampo = true
		}
	}
	if temCampo {
		campos = append(campos, atual.String())
	}
	return campos
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

// programaDoPlist lê o programa (o primeiro item do ProgramArguments) de um
// plist gerado pelo agente. Vazio quando não há.
func programaDoPlist(conteudo string) string {
	_, resto, ok := strings.Cut(conteudo, "<key>ProgramArguments</key>")
	if !ok {
		return ""
	}
	_, resto, ok = strings.Cut(resto, "<string>")
	if !ok {
		return ""
	}
	v, _, ok := strings.Cut(resto, "</string>")
	if !ok {
		return ""
	}
	return xmlUnescape(v)
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
	if err := ValidarNome(opts.Name); err != nil {
		return nil, err
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

	if err := ConferirPrograma(opts.Scope, opts.BinaryPath); err != nil {
		return nil, err
	}

	inst, err := installPlatform(ctx, cfg, opts)
	if inst != nil {
		inst.Programa = opts.BinaryPath
	}
	return inst, err
}

// Regravar reescreve o registro de um serviço já instalado com o modelo desta
// versão do agente, sem habilitar nem reiniciar. Devolve o arquivo regravado,
// ou "" na plataforma em que não há o que regravar. Ver regravarPlatform
// (systemd.go).
func Regravar(ctx context.Context, cfg *config.Config, opts Options) (string, error) {
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	if !nameRe.MatchString(opts.Name) {
		return "", fmt.Errorf("nome de serviço inválido %q", opts.Name)
	}
	if opts.Scope == "" {
		opts.Scope = defaultScope()
	}
	if opts.Scope != ScopeSystem && opts.Scope != ScopeUser {
		return "", fmt.Errorf("escopo inválido %q: use %q ou %q", opts.Scope, ScopeSystem, ScopeUser)
	}
	if opts.BinaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("descobrindo o caminho do próprio binário: %w", err)
		}
		opts.BinaryPath = exe
	}
	if cfg.ConfigPath == "" {
		return "", fmt.Errorf("não sei qual env-file o serviço carrega")
	}
	if err := ConferirPrograma(opts.Scope, opts.BinaryPath); err != nil {
		return "", err
	}
	return regravarPlatform(ctx, cfg, opts)
}

// ConferirAuxiliar recusa um programa que este processo vai rodar como
// administrador — o RustFS da cópia local, filho do serviço — se quem não é
// administrador pode trocá-lo: viraria administrador da máquina no próximo
// início. Com o processo sem privilégio (o escopo user), não há o que
// escalar e não confere.
func ConferirAuxiliar(programa string) error {
	if EscopoEfetivo("") != ScopeSystem {
		return nil
	}
	return conferirProgramaCom(programa, func(abs, problema string) error {
		return fmt.Errorf("o agente roda como administrador e chamaria %s, mas %s: quem pode trocar esse arquivo viraria administrador desta máquina. Deixe a pasta do programa só para o administrador e rode de novo o local-storage install", abs, problema)
	})
}

// ConferirPrograma recusa registrar no escopo do sistema (root; no Windows,
// LocalSystem) um programa que outro usuário pode trocar — ver
// conferirPrograma (programa_unix.go, programa_windows.go). No escopo user não
// há o que conferir: o serviço roda como o próprio dono do programa. Escopo
// vazio = o padrão desta plataforma; programa vazio = este executável. O
// install a chama antes do registro no painel, para não deixar um servidor
// aprovado sem serviço.
func ConferirPrograma(scope Scope, programa string) error {
	if EscopoEfetivo(scope) != ScopeSystem {
		return nil
	}
	if programa == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("descobrindo o caminho do próprio binário: %w", err)
		}
		programa = exe
	}
	return conferirPrograma(programa)
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
