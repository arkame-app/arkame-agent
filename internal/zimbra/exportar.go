package zimbra

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
)

// MesesDeTodas: no modo "todas as mensagens", quantos meses saem mês a mês; o
// que é mais antigo vai num arquivo só (anteriores.tgz), exportado uma vez.
const MesesDeTodas = 24

// MarcadorDeFalha: o arquivo que fica na pasta da conta cuja última
// exportação falhou. O painel vê o nome (e mostra a conta com falha); o
// conteúdo, com o motivo, fica no servidor.
const MarcadorDeFalha = "exportacao-falhou"

// MarcadorForaDoBackup: a pasta de uma conta que a exportação deixou de levar
// (apagada do Zimbra, renomeada, posta em --exclude). Os arquivos ficam — são
// o backup dela —, e o painel mostra que não estão mais sendo atualizados.
const MarcadorForaDoBackup = "fora-do-backup"

// maxSemAvanco: em quantas sessões seguidas uma conta pode ficar pendente sem
// ganhar arquivo novo antes de virar falha (o prazo é curto para ela).
const maxSemAvanco = 3

// folgaDoPrazo: quanto antes do prazo do comando a exportação para de começar
// meses, para gravar o inventário e sair com tempo.
const folgaDoPrazo = time.Minute

// Opcoes da exportação.
type Opcoes struct {
	// Pasta da exportação (fica fechada para o administrador).
	Pasta string
	// Anos: 0 = todas as mensagens; N = só as dos últimos N anos.
	Anos int
	// Contas: só estas (vazio = todas as do servidor). Excluir: menos estas.
	Contas, Excluir []string
	// Prazo: quando o comando tem de ter terminado (o plano o passa em
	// ARKAME_HOOK_DEADLINE). A exportação para antes e deixa o resto para a
	// próxima sessão. Zero: sem prazo.
	Prazo time.Time
	// Agora: o relógio (os testes o fixam). Zero: time.Now().
	Agora time.Time
}

// ResultadoDaConta é o que o inventário diz de uma conta.
type ResultadoDaConta struct {
	Conta string `json:"conta"`
	Meses int    `json:"meses"`
	Bytes int64  `json:"bytes"`
	// Exportado: quantos arquivos de mensagens saíram nesta sessão.
	Exportado int `json:"meses_exportados_agora"`
	// Novos: quantos destes a pasta ainda não tinha (é o que conta como avanço).
	Novos int `json:"arquivos_novos,omitempty"`
	// UltimoSucesso: a última exportação da conta que foi até o fim.
	UltimoSucesso time.Time `json:"ultimo_sucesso,omitzero"`
	// UltimaTentativa: a última sessão que começou esta conta (a fila anda por ela).
	UltimaTentativa time.Time `json:"ultima_tentativa,omitzero"`
	// SemAvanco: sessões seguidas em que o prazo acabou sem arquivo novo.
	SemAvanco int `json:"sessoes_sem_avanco,omitempty"`
	// Conferido: o último arquivo fechado conferido de novo (rodízio).
	Conferido string `json:"ultimo_conferido,omitempty"`
	// Corte: o mês antes do qual vai tudo para anteriores.tgz (modo "todas").
	// Daí até o corrente, todo mês tem arquivo próprio — mesmo os que a janela
	// de 24 meses já deixou para trás.
	Corte string `json:"corte_das_anteriores,omitempty"`
	// interrompida: o prazo venceu no meio do obrigatório (só nesta sessão).
	interrompida bool
	// erroNoRodizio: a falha é só da conferência (opcional): o obrigatório
	// saiu, e ela não pode derrubar o backup.
	erroNoRodizio bool
	// Pendente: o prazo acabou antes de a conta terminar; continua na próxima.
	Pendente bool   `json:"pendente,omitempty"`
	Erro     string `json:"erro,omitempty"`
}

// Inventario é o inventario.json da pasta: o que a exportação levou. Sem
// assunto, sem remetente — só contas, meses e tamanhos.
type Inventario struct {
	GeradoEm  time.Time          `json:"gerado_em"`
	Anos      int                `json:"anos"`
	Contas    []ResultadoDaConta `json:"contas"`
	Falhas    int                `json:"falhas"`
	Pendentes int                `json:"pendentes,omitempty"`
	// SessoesSemDisco: sessões seguidas em que o disco não deixou exportar
	// nada. Com a exportação anterior na pasta, o backup segue com ela; depois
	// de maxSemAvanco, o comando falha (senão o plano ficaria verde para
	// sempre com e-mail velho).
	SessoesSemDisco int `json:"sessoes_sem_disco,omitempty"`
}

// jaExportou: alguma conta já teve exportação completa nesta pasta. É o que
// separa "o backup leva a anterior" de "o plano subiria vazio".
func jaExportou(anterior map[string]ResultadoDaConta) bool {
	for _, c := range anterior {
		if !c.UltimoSucesso.IsZero() {
			return true
		}
	}
	return false
}

// semDiscoDeNovo conta mais uma sessão sem exportar por falta de disco, grava
// e devolve o erro quando já são maxSemAvanco seguidas.
func semDiscoDeNovo(pasta string, inv *Inventario, motivo string) error {
	inv.SessoesSemDisco = inventarioAnteriorOuVazio(pasta).SessoesSemDisco + 1
	if err := gravarJSON(filepath.Join(pasta, "inventario.json"), inv); err != nil {
		return err
	}
	if inv.SessoesSemDisco >= maxSemAvanco {
		return fmt.Errorf("há %d sessões o disco não deixa exportar o Zimbra: %s", inv.SessoesSemDisco, motivo)
	}
	return nil
}

// mes é o primeiro instante (UTC) de um mês.
type mes time.Time

func (m mes) nome() string  { return time.Time(m).Format("2006-01") }
func (m mes) seguinte() mes { return mes(time.Time(m).AddDate(0, 1, 0)) }

func inicioDoMes(t time.Time) mes {
	t = t.UTC()
	return mes(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC))
}

// mesesDaJanela: do mais antigo ao corrente. "Os últimos N anos" a partir de
// hoje começam no meio do mês de N anos atrás: são N×12 meses e mais aquele.
func mesesDaJanela(agora time.Time, anos int) []mes {
	atual := inicioDoMes(agora)
	n := MesesDeTodas
	if anos > 0 {
		n = anos*12 + 1
	}
	var ms []mes
	for i := n - 1; i >= 0; i-- {
		ms = append(ms, mes(time.Time(atual).AddDate(0, -i, 0)))
	}
	return ms
}

// A busca do Zimbra entende a data em milissegundos (sem depender do idioma
// da conta: "dd/mm/aaaa" numa conta em português dá erro 500, e "mm/dd" é
// lido no fuso da conta). Medido no 8.8.15: a comparação é ao segundo e
// estrita nas duas pontas — `after:X before:Y` é X < data < Y.
func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// consultaDoMes: as mensagens de [início do mês, início do seguinte), em
// qualquer pasta (sem `is:anywhere` a busca deixa de fora a Lixeira e o Spam).
func consultaDoMes(m mes) string {
	return "is:anywhere after:" + ms(time.Time(m).Add(-time.Second)) + " before:" + ms(time.Time(m.seguinte()))
}

// consultaAnteriores: tudo o que é anterior ao mês dado.
func consultaAnteriores(m mes) string {
	return "is:anywhere before:" + ms(time.Time(m))
}

// tiposDaEstrutura: o que não é mensagem e vai inteiro, a cada sessão, em
// estrutura.tgz (a data de um contato é a da última mudança: por mês, ele
// trocaria de arquivo).
const tiposDaEstrutura = "contact,appointment,task,document,wiki"

var reArquivoDoMes = regexp.MustCompile(`^caixa-(\d{4}-\d{2})\.(tgz|vazio)$`)

// Exportar exporta as contas para a pasta e grava o inventário. Uma conta que
// falha não derruba as outras: fica no inventário com o erro, e o resumo a
// cita. Só a falha em listar as contas (ou em todas) é erro. Prazo vencido
// não é erro: o que faltou fica para a próxima sessão, começando por ele.
func Exportar(ctx context.Context, c *Cliente, o Opcoes, log io.Writer) (*Inventario, error) {
	if o.Agora.IsZero() {
		o.Agora = time.Now()
	}
	if o.Anos < 0 || o.Anos > 30 {
		return nil, fmt.Errorf("--years tem de ser de 0 (todas) a 30")
	}
	if !o.Prazo.IsZero() && time.Until(o.Prazo) <= folgaDoPrazo {
		return nil, fmt.Errorf("o prazo do comando é curto demais para exportar o Zimbra: aumente-o no plano (até 2 h)")
	}
	if err := segredo.PastaPrivada(o.Pasta); err != nil {
		return nil, fmt.Errorf("pasta da exportação: %w", err)
	}
	soltar, err := travar(o.Pasta)
	if err != nil {
		return nil, err
	}
	defer soltar()
	guarda, semEspacoNoComeco := novaGuarda(o.Pasta)
	if semEspacoNoComeco != "" {
		// Sem a exportação de hoje, o backup do que já estava exportado
		// segue: falhar aqui derrubaria o plano todo, todo dia. Sem nenhuma
		// exportação completa ainda, é erro (o plano subiria vazio), e
		// depois de algumas sessões seguidas, também.
		if !jaExportou(inventarioAnterior(o.Pasta)) {
			return nil, errors.New(semEspacoNoComeco)
		}
		fmt.Fprintf(log, "NÃO EXPORTEI: %s. O backup leva a exportação anterior.\n", semEspacoNoComeco)
		inv := inventarioAnteriorOuVazio(o.Pasta)
		if err := semDiscoDeNovo(o.Pasta, inv, semEspacoNoComeco); err != nil {
			return inv, err
		}
		return inv, nil
	}
	listadas, err := c.ListarContas(ctx)
	if err != nil {
		return nil, err
	}
	existentes := map[string]bool{}
	for _, l := range listadas {
		existentes[l.Nome] = true
	}
	listadas = filtrar(listadas, o.Contas, o.Excluir)
	if len(listadas) == 0 {
		return nil, fmt.Errorf("nenhuma conta para exportar (confira --accounts e --exclude)")
	}
	anterior := inventarioAnterior(o.Pasta)
	// Primeiro a que está há mais tempo sem ser tentada (a que nunca foi,
	// antes de todas): com o prazo curto e muitas contas, cada sessão avança
	// por contas diferentes. Pela última tentativa, e não pelo último sucesso:
	// uma conta que nunca termina no prazo voltaria sempre ao começo da fila e
	// seguraria todas as outras.
	sort.SliceStable(listadas, func(i, j int) bool {
		a, b := anterior[listadas[i].Nome].UltimaTentativa, anterior[listadas[j].Nome].UltimaTentativa
		if !a.Equal(b) {
			return a.Before(b)
		}
		return listadas[i].Nome < listadas[j].Nome
	})

	trabalho := ctx
	if !o.Prazo.IsZero() {
		var cancelar context.CancelFunc
		trabalho, cancelar = context.WithDeadline(ctx, o.Prazo.Add(-folgaDoPrazo))
		defer cancelar()
	}
	trabalho, pararDeVigiar := guarda.vigiar(trabalho)
	defer pararDeVigiar()
	avancou := false
	inv := &Inventario{GeradoEm: o.Agora.UTC(), Anos: o.Anos}
	levadas, vistas := map[string]bool{}, map[string]bool{}
	for _, l := range listadas {
		vistas[l.Nome] = true
		if l.Motivo != "" {
			// O que o inventário sabia dela fica (o corte, o rodízio): a caixa
			// pode voltar para este servidor.
			r := anterior[l.Nome]
			r.Conta, r.Erro, r.Pendente, r.Exportado, r.Novos = l.Nome, "não exportada: "+l.Motivo, false, 0, 0
			inv.Contas = append(inv.Contas, r)
			inv.Falhas++
			fmt.Fprintf(log, "FICOU DE FORA %s: %s\n", l.Nome, l.Motivo)
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		levadas[l.Nome] = true
		ant := anterior[l.Nome]
		r := ResultadoDaConta{Conta: l.Nome, UltimoSucesso: ant.UltimoSucesso, UltimaTentativa: ant.UltimaTentativa, SemAvanco: ant.SemAvanco, Conferido: ant.Conferido, Corte: ant.Corte}
		if trabalho.Err() == nil {
			r.UltimaTentativa = o.Agora.UTC()
			r = exportarConta(trabalho, c, o, l.Nome, r)
			// Avançou: terminou a conta, ou exportou um arquivo que a pasta
			// não tinha (`Novos`). O mês corrente reexportado (`Exportado`)
			// não conta: numa caixa que recebe e-mail ele muda todo dia, e um
			// mês antigo que nunca cabe ficaria pendente para sempre, com o
			// backup verde (revisão, 17/10). É o critério do prazo.
			if r.Novos > 0 || (!r.interrompida && r.Erro == "") {
				avancou = true
			}
			if r.interrompida && r.Erro == "" && ctx.Err() == nil && guarda.parou() {
				// Parada pelo disco: pendência; a conta que nunca cabe vira
				// falha depois de algumas sessões, como a do prazo.
				r.Pendente = true
				if r.Novos > 0 {
					r.SemAvanco = 0
					os.Remove(filepath.Join(o.Pasta, "contas", l.Nome, MarcadorDeFalha))
				} else {
					if r.SemAvanco++; r.SemAvanco >= maxSemAvanco {
						r.Pendente = false
						r.Erro = fmt.Sprintf("há %d sessões a conta não cabe no disco de %s: libere espaço ou use --dir num disco maior", r.SemAvanco, o.Pasta)
						_ = segredo.Gravar(filepath.Join(o.Pasta, "contas", l.Nome, MarcadorDeFalha), []byte(r.Erro+"\n"), 0o600)
					}
				}
			} else if r.interrompida && r.Erro == "" && ctx.Err() == nil {
				// O prazo do comando venceu no meio: não é falha, a não ser
				// que a conta não avance há várias sessões.
				r.Pendente = true
				marcador := filepath.Join(o.Pasta, "contas", l.Nome, MarcadorDeFalha)
				if r.Novos > 0 {
					r.SemAvanco = 0
					os.Remove(marcador)
				} else if r.SemAvanco++; r.SemAvanco >= maxSemAvanco {
					r.Pendente = false
					r.Erro = fmt.Sprintf("há %d sessões o prazo do comando acaba antes de esta conta avançar: aumente o prazo no plano (até 2 h) ou exporte só os últimos anos", r.SemAvanco)
					_ = segredo.Gravar(marcador, []byte(r.Erro+"\n"), 0o600)
				}
			}
		} else {
			r.Pendente = true
		}
		resumirDoDisco(filepath.Join(o.Pasta, "contas", l.Nome), &r)
		switch {
		case r.Pendente:
			inv.Pendentes++
		case r.Erro != "":
			inv.Falhas++
			fmt.Fprintf(log, "FALHOU %s: %s\n", l.Nome, r.Erro)
		default:
			r.UltimoSucesso, r.SemAvanco = o.Agora.UTC(), 0
		}
		inv.Contas = append(inv.Contas, r)
	}
	// A guarda parou no laço das contas? (No rodízio, que é opcional, ela só
	// encurta a conferência: não é erro.)
	paradaNasContas := guarda.parou()
	// O rodízio (opcional) é uma segunda passada, com o tempo que sobrou: a
	// conferência de um arquivo grande de uma conta não pode tirar o mês
	// corrente das contas que vêm depois.
	for i := range inv.Contas {
		r := &inv.Contas[i]
		if trabalho.Err() != nil {
			break
		}
		if levadas[r.Conta] && !r.Pendente && r.Erro == "" {
			if conferirUmArquivo(trabalho, c, o, r); r.erroNoRodizio {
				inv.Falhas++
				fmt.Fprintf(log, "FALHOU %s: %s\n", r.Conta, r.Erro)
			}
		}
	}
	// Uma exportação com --accounts (à mão, para uma conta) não apaga o que o
	// inventário sabe das outras.
	if len(o.Contas) > 0 {
		for nome, ant := range anterior {
			if !vistas[nome] && existentes[nome] {
				inv.Contas = append(inv.Contas, ant)
			}
		}
		sort.Slice(inv.Contas, func(i, j int) bool { return inv.Contas[i].Conta < inv.Contas[j].Conta })
	}
	pararDeVigiar()
	marcarForaDoBackup(o.Pasta, levadas, existentes, o.Excluir)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var semEspaco error
	if guarda.parou() {
		fmt.Fprintf(log, "PAROU para não encher o disco de %s (%s livres; a exportação deixa %s e metade do resto para o backup): o que faltou fica para a próxima sessão\n",
			o.Pasta, bytesLegiveis(guarda.livreNaParada.Load()), bytesLegiveis(guarda.reserva))
		// Erro só quando nunca houve exportação (o plano subiria vazio): com
		// a anterior na pasta, o backup a leva, e o que faltou fica para a
		// próxima sessão. Falhar aqui derrubaria o backup do dia.
		if paradaNasContas && !avancou {
			motivo := fmt.Sprintf("a exportação do Zimbra não coube no disco de %s: nenhuma conta avançou nesta sessão. Libere espaço ou use --dir numa pasta de um disco maior (e ponha essa pasta no plano)", o.Pasta)
			if !jaExportou(anterior) {
				semEspaco = errors.New(motivo)
			} else if inv.SessoesSemDisco = inventarioAnteriorOuVazio(o.Pasta).SessoesSemDisco + 1; inv.SessoesSemDisco >= maxSemAvanco {
				semEspaco = fmt.Errorf("há %d sessões %s", inv.SessoesSemDisco, motivo)
			}
		}
	}
	if err := gravarJSON(filepath.Join(o.Pasta, "inventario.json"), inv); err != nil {
		return nil, err
	}
	ok, saiu := 0, 0
	for _, r := range inv.Contas {
		if levadas[r.Conta] {
			saiu += r.Exportado
			if !r.Pendente && (r.Erro == "" || r.erroNoRodizio) {
				ok++
			}
		}
	}
	fmt.Fprintf(log, "Zimbra: %d conta(s) exportada(s), %d com falha (%s)\n", ok, inv.Falhas, descricaoDoModo(o.Anos))
	if inv.Pendentes > 0 && !guarda.parou() {
		fmt.Fprintf(log, "o prazo do comando acabou: %d conta(s) continuam na próxima sessão (aumente o prazo no plano se for sempre assim)\n", inv.Pendentes)
	}
	// Erro (que aborta o backup) só quando nada saiu: uma conta com falha não
	// pode impedir que as outras — e o e-mail de hoje — subam. A falha fica
	// no marcador da conta, que o painel mostra.
	if ok == 0 && inv.Pendentes == 0 && saiu == 0 {
		return inv, fmt.Errorf("nenhuma conta foi exportada")
	}
	if semEspaco != nil {
		return inv, semEspaco
	}
	return inv, nil
}

func descricaoDoModo(anos int) string {
	if anos == 0 {
		return "todas as mensagens"
	}
	return fmt.Sprintf("mensagens dos últimos %d ano(s)", anos)
}

func filtrar(contas []ContaListada, so, menos []string) []ContaListada {
	norm := func(l []string) []string {
		var r []string
		for _, c := range l {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				r = append(r, c)
			}
		}
		return r
	}
	so, menos = norm(so), norm(menos)
	var r []ContaListada
	for _, c := range contas {
		if (len(so) > 0 && !slices.Contains(so, c.Nome)) || slices.Contains(menos, c.Nome) {
			continue
		}
		r = append(r, c)
	}
	return r
}

// inventarioAnteriorOuVazio: o inventário que já está na pasta, para a sessão
// que não exportou nada (sem espaço) devolver o estado de antes.
func inventarioAnteriorOuVazio(pasta string) *Inventario {
	inv := &Inventario{}
	if b, err := os.ReadFile(filepath.Join(pasta, "inventario.json")); err == nil {
		_ = json.Unmarshal(b, inv)
	}
	return inv
}

// inventarioAnterior: o que a sessão passada disse de cada conta.
func inventarioAnterior(pasta string) map[string]ResultadoDaConta {
	r := map[string]ResultadoDaConta{}
	b, err := os.ReadFile(filepath.Join(pasta, "inventario.json"))
	if err != nil {
		return r
	}
	var inv Inventario
	if json.Unmarshal(b, &inv) == nil {
		for _, c := range inv.Contas {
			r[c.Conta] = c
		}
	}
	return r
}

// atributosSecretos: o que não sai da conta, nem que o Zimbra entregue (as
// propriedades de zimlet guardam credenciais de serviços de terceiros).
var atributosSecretos = regexp.MustCompile(`(?i)password|secret|token|scratch|credential|oauth|zimletuserprop`)

// configuracaoDaConta é o conta.json: os atributos da própria conta (sem o
// que vem da classe de serviço, que a recriação herda de novo) e a classe de
// serviço pelo nome (o id muda de um servidor para outro).
type configuracaoDaConta struct {
	Conta       string    `json:"conta"`
	Classe      string    `json:"classe_de_servico,omitempty"`
	Atributos   atributos `json:"atributos"`
	ExportadoEm time.Time `json:"exportado_em"`
}

// exportarConta grava a configuração, os filtros, as assinaturas, a estrutura
// e as mensagens de uma conta. Um mês que falha não para os outros: a conta
// sai com o erro, e o marcador de falha fica na pasta. O prazo que vence no
// meio do obrigatório não é erro: r.interrompida diz que a conta ficou pela
// metade (Exportar decide se é pendência ou falha).
func exportarConta(ctx context.Context, c *Cliente, o Opcoes, conta string, r ResultadoDaConta) ResultadoDaConta {
	dir := filepath.Join(o.Pasta, "contas", conta)
	var erros []string
	falhou := func(oque string, err error) {
		if ctx.Err() != nil {
			r.interrompida = true
			return
		}
		erros = append(erros, oque+err.Error())
	}
	terminar := func() ResultadoDaConta {
		r.Erro = fecharMarcador(dir, erros, r.interrompida)
		return r
	}
	if err := segredo.PastaPrivada(dir); err != nil {
		falhou("", err)
		return terminar()
	}
	// O que sobrou de uma exportação derrubada no meio sai antes de subir no
	// backup. O que já estava completo fica, e esta sessão continua dos meses
	// que faltam — a primeira exportação de um servidor grande pode levar
	// várias sessões.
	if sobras, _ := filepath.Glob(filepath.Join(dir, ".*.parcial")); len(sobras) > 0 {
		for _, s := range sobras {
			os.Remove(s)
		}
	}
	if err := exportarConfiguracao(ctx, c, o, conta, dir); err != nil {
		falhou("", err)
		return terminar()
	}
	exportar := func(base string, params url.Values, soSeMudou bool) error {
		novo := !existe(dir, base+".tgz") && !existe(dir, base+".vazio")
		trocou, err := exportarMensagens(ctx, c, o, conta, dir, base, params, soSeMudou, strings.HasPrefix(base, "caixa-") && !fechado(dir, base))
		if err == nil && trocou {
			r.Exportado++
			if novo {
				r.Novos++
			}
		}
		return err
	}

	// Contatos, agenda e documentos: na primeira vez, antes de tudo (não
	// podem ficar presos atrás de meses que levam várias sessões); depois,
	// logo após os meses (com documentos grandes, o download inteiro a cada
	// sessão não pode tirar o lugar do e-mail de hoje). O arquivo só é trocado
	// (e sobe de novo) se algo mudou.
	estrutura := func() {
		if ctx.Err() == nil {
			if err := exportar("estrutura", url.Values{"types": {tiposDaEstrutura}, "query": {"is:anywhere"}}, true); err != nil {
				falhou("contatos, agenda e documentos: ", err)
			}
		}
	}
	primeira := !existe(dir, "estrutura.tgz") && !existe(dir, "estrutura.vazio")
	if primeira {
		estrutura()
	}

	// Mensagens, do mês corrente para trás: com o prazo curto, o recente
	// (o que a pessoa mais procura) sai primeiro. O mês que ainda corre só é
	// trocado se mudou (sem mensagem nova, não sobe de novo), mas a data dele
	// avança: é por ela que o mês, um dia, fica fechado.
	for _, m := range mesesAExportar(o, dir, r.Corte) {
		if ctx.Err() != nil {
			break
		}
		if mesFechado(dir, m) {
			continue
		}
		if err := exportar("caixa-"+m.nome(), url.Values{"types": {"message"}, "query": {consultaDoMes(m)}}, true); err != nil {
			falhou(fmt.Sprintf("mês %s: ", m.nome()), err)
		}
	}
	if !primeira {
		estrutura()
	}
	if ctx.Err() != nil {
		r.interrompida = true
		return terminar()
	}
	janela := mesesDaJanela(o.Agora, o.Anos)
	if o.Anos == 0 {
		if !existe(dir, "anteriores.tgz") && !existe(dir, "anteriores.vazio") {
			corte := corteDasAnteriores(dir, janela)
			if err := exportar("anteriores", url.Values{"types": {"message"}, "query": {consultaAnteriores(corte)}}, false); err != nil {
				falhou(fmt.Sprintf("mensagens anteriores a %s: ", corte.nome()), err)
			} else {
				r.Corte = corte.nome()
			}
		} else if r.Corte == "" {
			// Exportação de antes do campo: o corte foi o mês mais antigo.
			r.Corte = corteDasAnteriores(dir, janela).nome()
		}
	} else {
		r.Corte = ""
		if err := removerAntesDe(dir, janela[0].nome()); err != nil {
			// Fora da janela: sai da pasta (o que já subiu fica pela retenção).
			falhou("", err)
		}
	}
	if ctx.Err() != nil {
		r.interrompida = true
	}
	return terminar()
}

// mesesAExportar: do corrente para trás, os meses que têm arquivo próprio — a
// janela e, no modo "todas", também os de antes dela até o corte das
// anteriores (um mês que falhou quando ainda estava na janela não pode sumir
// do backup quando ela anda).
func mesesAExportar(o Opcoes, dir, corte string) []mes {
	janela := mesesDaJanela(o.Agora, o.Anos)
	if o.Anos == 0 {
		inicio := janela[0]
		if t, err := time.Parse("2006-01", corte); err == nil && time.Time(mes(t)).Before(time.Time(inicio)) {
			inicio = mes(t)
		} else if m, ok := mesMaisAntigo(dir); ok && corte == "" && time.Time(m).Before(time.Time(inicio)) {
			inicio = m
		}
		var ms []mes
		for m := inicio; !time.Time(m).After(time.Time(janela[len(janela)-1])); m = m.seguinte() {
			ms = append(ms, m)
		}
		janela = ms
	}
	slices.Reverse(janela)
	return janela
}

// conferirUmArquivo: o rodízio. Um arquivo fechado por sessão é conferido de
// novo: uma caixa migrada depois (imapsync, importação) traz mensagens com
// data antiga, que cairiam num arquivo que a exportação não refaz. Só troca se
// mudou. Opcional: o prazo que vence aqui não muda o resultado da conta, e o
// rodízio anda mesmo assim (um arquivo grande demais para o prazo não prende
// os outros).
func conferirUmArquivo(ctx context.Context, c *Cliente, o Opcoes, r *ResultadoDaConta) {
	dir := filepath.Join(o.Pasta, "contas", r.Conta)
	janela := mesesDaJanela(o.Agora, o.Anos)
	base, ok := proximoAConferir(dir, janela, r.Conferido, o.Agora, o.Anos == 0)
	if !ok {
		return
	}
	r.Conferido = base
	params := url.Values{"types": {"message"}}
	if base == "anteriores" {
		params.Set("query", consultaAnteriores(corteDasAnteriores(dir, janela)))
		if t, err := time.Parse("2006-01", r.Corte); err == nil {
			params.Set("query", consultaAnteriores(mes(t)))
		}
	} else if m, err := time.Parse("2006-01", strings.TrimPrefix(base, "caixa-")); err == nil {
		params.Set("query", consultaDoMes(mes(m)))
	}
	trocou, err := exportarMensagens(ctx, c, o, r.Conta, dir, base, params, true, false)
	switch {
	case err != nil && ctx.Err() == nil:
		r.Erro, r.erroNoRodizio = fmt.Sprintf("conferindo %s: %v", base, err), true
		_ = segredo.Gravar(filepath.Join(dir, MarcadorDeFalha), []byte(r.Erro+"\n"), 0o600)
	case trocou:
		r.Exportado++
	}
}

// corteDasAnteriores: antes do mês mais antigo que a pasta tem — quem veio do
// modo "N anos" já tem meses além dos 24, e eles não podem sair duas vezes.
func corteDasAnteriores(dir string, janela []mes) mes {
	corte := janela[0]
	if m, ok := mesMaisAntigo(dir); ok && time.Time(m).Before(time.Time(corte)) {
		corte = m
	}
	return corte
}

// proximoAConferir: o arquivo fechado seguinte ao último conferido, em volta —
// os meses fechados da pasta (os da janela e os mais antigos que ficaram) e,
// no modo "todas", o das anteriores, por último. O que saiu nesta mesma
// sessão não precisa.
func proximoAConferir(dir string, janela []mes, ultimo string, agora time.Time, comAnteriores bool) (string, bool) {
	vistos := map[string]bool{}
	var bases []string
	add := func(m mes) {
		if b := "caixa-" + m.nome(); !vistos[b] && mesFechado(dir, m) && !feitoAgora(dir, m, agora) {
			vistos[b] = true
			bases = append(bases, b)
		}
	}
	entradas, _ := os.ReadDir(dir)
	for _, e := range entradas {
		if x := reArquivoDoMes.FindStringSubmatch(e.Name()); x != nil {
			if t, err := time.Parse("2006-01", x[1]); err == nil {
				add(mes(t))
			}
		}
	}
	for _, m := range janela {
		add(m)
	}
	sort.Strings(bases)
	// As anteriores entram também quando saíram vazias: a conta que começou
	// sem histórico e recebeu uma caixa migrada depois é o caso que mais
	// precisa (medido no Zimbra real).
	for _, ext := range []string{".tgz", ".vazio"} {
		if fi, err := os.Stat(filepath.Join(dir, "anteriores"+ext)); comAnteriores && err == nil && !fi.ModTime().Equal(agora) {
			bases = append(bases, "anteriores")
			break
		}
	}
	if len(bases) == 0 {
		return "", false
	}
	if i := slices.Index(bases, ultimo); i >= 0 {
		return bases[(i+1)%len(bases)], true
	}
	// Nunca conferido (ou o último saiu da pasta): o primeiro depois dele.
	for _, b := range bases {
		if b > ultimo && b != "anteriores" {
			return b, true
		}
	}
	return bases[0], true
}

// marcarForaDoBackup marca as pastas das contas que saíram do Zimbra ou que o
// plano exclui, e desmarca as que esta exportação levou. A que só ficou de
// fora de um --accounts (uma exportação à mão) fica como estava.
func marcarForaDoBackup(pasta string, levadas, existentes map[string]bool, excluir []string) {
	entradas, _ := os.ReadDir(filepath.Join(pasta, "contas"))
	for _, e := range entradas {
		if !e.IsDir() {
			continue
		}
		marcador := filepath.Join(pasta, "contas", e.Name(), MarcadorForaDoBackup)
		switch {
		case levadas[e.Name()]:
			os.Remove(marcador)
		case existentes[e.Name()] && !slices.ContainsFunc(excluir, func(x string) bool { return strings.EqualFold(strings.TrimSpace(x), e.Name()) }):
		case !existe(filepath.Join(pasta, "contas", e.Name()), MarcadorForaDoBackup):
			_ = segredo.Gravar(marcador, []byte("a exportação não leva mais esta conta (apagada do Zimbra, renomeada ou fora do plano); os arquivos são o último backup dela\n"), 0o600)
		}
	}
}

// fecharMarcador põe ou tira o marcador de falha da conta e devolve o erro.
// Com erro de verdade, põe (mesmo que o prazo tenha vencido depois). Sem erro
// e completa, tira. Interrompida pelo prazo sem erro, deixa: quem decide é
// Exportar (pendente que avançou tira; a que não avança há sessões, põe).
func fecharMarcador(dir string, erros []string, interrompida bool) string {
	marcador := filepath.Join(dir, MarcadorDeFalha)
	if len(erros) > 0 {
		msg := strings.Join(erros, "; ")
		_ = segredo.Gravar(marcador, []byte(msg+"\n"), 0o600)
		return msg
	}
	if !interrompida {
		os.Remove(marcador)
	}
	return ""
}

// exportarConfiguracao grava conta.json, os filtros e as assinaturas.
func exportarConfiguracao(ctx context.Context, c *Cliente, o Opcoes, conta, dir string) error {
	var ga struct {
		Account []contaDoDiretorio `json:"account"`
	}
	if err := c.admin(ctx, "GetAccountRequest", map[string]any{"applyCos": "0", "account": map[string]any{"by": "name", "_content": conta}}, &ga); err != nil {
		return fmt.Errorf("lendo a configuração: %w", err)
	}
	if len(ga.Account) != 1 {
		return errors.New("lendo a configuração: o Zimbra não devolveu a conta")
	}
	at := ga.Account[0].atributos()
	for n := range at {
		if atributosSecretos.MatchString(n) {
			delete(at, n)
		}
	}
	cfg := configuracaoDaConta{Conta: conta, Atributos: at, ExportadoEm: o.Agora.UTC()}
	if id := at.um("zimbraCOSId"); id != "" {
		var cos struct {
			Cos []struct {
				Name string `json:"name"`
			} `json:"cos"`
		}
		if err := c.admin(ctx, "GetCosRequest", map[string]any{"cos": map[string]any{"by": "id", "_content": id}}, &cos); err == nil && len(cos.Cos) == 1 {
			cfg.Classe = cos.Cos[0].Name
		}
	}
	if err := gravarJSON(filepath.Join(dir, "conta.json"), cfg); err != nil {
		return err
	}
	for arquivo, nome := range map[string]string{"filtros.sieve": "zimbraMailSieveScript", "filtros-saida.sieve": "zimbraMailOutgoingSieveScript"} {
		if err := segredo.Gravar(filepath.Join(dir, arquivo), []byte(at.um(nome)), 0o600); err != nil {
			return err
		}
	}
	var sig struct {
		Signature []json.RawMessage `json:"signature"`
	}
	if err := c.daConta(ctx, conta, "GetSignaturesRequest", nil, &sig); err != nil {
		return fmt.Errorf("lendo as assinaturas: %w", err)
	}
	if sig.Signature == nil {
		sig.Signature = []json.RawMessage{}
	}
	return gravarJSON(filepath.Join(dir, "assinaturas.json"), sig.Signature)
}

// mesFechado: o arquivo do mês existe e foi gerado depois que o mês acabou
// (com um dia de folga para o que chega atrasado). Um mês exportado enquanto
// ainda corria é refeito, mesmo que o agente tenha ficado meses parado.
func mesFechado(dir string, m mes) bool {
	limite := time.Time(m.seguinte()).Add(24 * time.Hour)
	for _, ext := range []string{".tgz", ".vazio"} {
		if fi, err := os.Stat(filepath.Join(dir, "caixa-"+m.nome()+ext)); err == nil && !fi.ModTime().Before(limite) {
			return true
		}
	}
	return false
}

// fechado: o mesFechado de um arquivo "caixa-AAAA-MM".
func fechado(dir, base string) bool {
	t, err := time.Parse("2006-01", strings.TrimPrefix(base, "caixa-"))
	return err == nil && mesFechado(dir, mes(t))
}

func feitoAgora(dir string, m mes, agora time.Time) bool {
	for _, ext := range []string{".tgz", ".vazio"} {
		if fi, err := os.Stat(filepath.Join(dir, "caixa-"+m.nome()+ext)); err == nil && fi.ModTime().Equal(agora) {
			return true
		}
	}
	return false
}

func existe(dir, nome string) bool {
	_, err := os.Stat(filepath.Join(dir, nome))
	return err == nil
}

// mesMaisAntigo: o mês do arquivo mais antigo da pasta.
func mesMaisAntigo(dir string) (mes, bool) {
	entradas, _ := os.ReadDir(dir)
	for _, e := range entradas { // ReadDir ordena pelo nome: o primeiro é o mais antigo
		if m := reArquivoDoMes.FindStringSubmatch(e.Name()); m != nil {
			if t, err := time.Parse("2006-01", m[1]); err == nil {
				return mes(t), true
			}
		}
	}
	return mes{}, false
}

func removerAntesDe(dir, inicio string) error {
	entradas, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entradas {
		m := reArquivoDoMes.FindStringSubmatch(e.Name())
		if (m != nil && m[1] < inicio) || e.Name() == "anteriores.tgz" || e.Name() == "anteriores.vazio" {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// resumirDoDisco conta os meses e os bytes que a pasta da conta tem.
func resumirDoDisco(dir string, r *ResultadoDaConta) {
	entradas, _ := os.ReadDir(dir)
	r.Meses, r.Bytes = 0, 0
	for _, e := range entradas {
		if reArquivoDoMes.MatchString(e.Name()) {
			r.Meses++
		}
		if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".tgz") {
			r.Bytes += fi.Size()
		}
	}
}

// exportarMensagens grava base.tgz com o que a consulta acha (ou base.vazio,
// sem nada). Primeiro num temporário: um export que caiu no meio não fica com
// o nome de um arquivo completo. A data do arquivo é a do começo da sessão
// (é por ela que mesFechado decide). soSeMudou: se o que está na pasta tem o
// mesmo conteúdo, fica como está (não sobe de novo); trocou diz se trocou.
// tocar: mesmo sem trocar, a data do arquivo passa a ser a da sessão (o mês
// que ainda corria, sem mensagem nova, fica fechado quando a sessão é depois
// do fim dele).
func exportarMensagens(ctx context.Context, c *Cliente, o Opcoes, conta, dir, base string, params url.Values, soSeMudou, tocar bool) (trocou bool, err error) {
	params.Set("fmt", "tgz")
	tmp, err := os.CreateTemp(dir, "."+base+"-*.parcial")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	err = c.baixar(ctx, conta, params, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	vazio := errors.Is(err, ErrSemConteudo)
	if err != nil && !vazio {
		return false, err
	}
	if soSeMudou && mesmoConteudo(dir, base, tmp.Name(), vazio) {
		if tocar {
			for _, ext := range []string{".tgz", ".vazio"} {
				if existe(dir, base+ext) {
					return false, os.Chtimes(filepath.Join(dir, base+ext), o.Agora, o.Agora)
				}
			}
		}
		return false, nil
	}
	final := filepath.Join(dir, base+".tgz")
	for _, ext := range []string{".tgz", ".vazio"} {
		os.Remove(filepath.Join(dir, base+ext))
	}
	if vazio {
		final = filepath.Join(dir, base+".vazio")
		if err := segredo.Gravar(final, nil, 0o600); err != nil {
			return false, err
		}
	} else if err := os.Rename(tmp.Name(), final); err != nil {
		return false, err
	}
	return true, os.Chtimes(final, o.Agora, o.Agora)
}

// mesmoConteudo: o que está na pasta tem as mesmas entradas, com o mesmo
// conteúdo, que o arquivo recém-baixado (o .tgz em si muda a cada exportação:
// o gzip leva a hora).
func mesmoConteudo(dir, base, novo string, novoVazio bool) bool {
	if existe(dir, base+".vazio") {
		return novoVazio
	}
	if novoVazio || !existe(dir, base+".tgz") {
		return false
	}
	a, errA := resumoDoTgz(filepath.Join(dir, base+".tgz"))
	b, errB := resumoDoTgz(novo)
	return errA == nil && errB == nil && slices.Equal(a, b)
}

// resumoDoTgz: nome e SHA-256 de cada entrada, em ordem de nome.
func resumoDoTgz(caminho string) ([]string, error) {
	f, err := os.Open(caminho)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var r []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		soma := sha256.New()
		if _, err := io.Copy(soma, tr); err != nil {
			return nil, err
		}
		r = append(r, h.Name+" "+hex.EncodeToString(soma.Sum(nil)))
	}
	sort.Strings(r)
	return r, nil
}

func gravarJSON(caminho string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return segredo.Gravar(caminho, append(b, '\n'), 0o600)
}
