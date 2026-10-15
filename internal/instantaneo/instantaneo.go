// Package instantaneo congela os volumes do plano antes do backup, para que os
// arquivos sejam lidos como estavam num mesmo instante.
//
// Sem isso, o agente lê o disco vivo. Arquivo aberto para escrita sai copiado
// no meio de uma gravação (a planilha que alguém está salvando, o banco do
// sistema de contabilidade), e o que o programa abre com trava exclusiva nem
// sai: o `.pst` do Outlook aberto dá "o arquivo está sendo usado por outro
// processo" em toda sessão, e o backup fica parcial para sempre.
//
// No Windows o instantâneo é o VSS (Volume Shadow Copy), o mesmo mecanismo do
// Backup do Windows e de quem faz backup profissional: o sistema avisa os
// programas que sabem participar (SQL Server, Exchange, Hyper-V), eles gravam o
// que estava pendente, o volume é congelado por alguns segundos, e a leitura
// passa a ser de uma cópia que não muda mais. O backup é do tipo cópia
// (VSS_BT_COPY): não mexe no histórico de backup de outros programas, como o
// log do SQL Server.
//
// O instantâneo é do contexto de backup: some sozinho quando o agente o solta,
// ou quando o processo morre. O agente nunca apaga cópias de sombra — apagar
// cópias de sombra é o primeiro passo de um ransomware, e o antivírus está
// certo em desconfiar de quem faz isso.
//
// Falhar ao criar o instantâneo não falha o backup: a leitura volta a ser do
// disco, como sempre foi, e o motivo vai ao painel (Aviso). Um backup do disco
// vivo é pior que um do instantâneo, mas é muito melhor que nenhum.
package instantaneo

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arkame-app/agent/internal/caminho"
)

// MetodoVSS é o consistency_method da sessão lida de um instantâneo do VSS.
const MetodoVSS = "vss"

// MetodoNenhum é o consistency_method da sessão do Windows que tentou o
// instantâneo e leu do disco.
const MetodoNenhum = "none"

// ModoDesligado é o valor do plano que dispensa o instantâneo.
const ModoDesligado = "off"

// PrazoDaCriacao limita a criação inteira: avisar os programas, congelar e
// criar. Na prática leva de 5 a 60 segundos; um escritor pendurado (um SQL
// Server que não responde) não pode segurar o backup do servidor.
const PrazoDaCriacao = 5 * time.Minute

// Instantaneo diz de onde ler os arquivos de uma sessão.
//
// O valor zero lê do disco.
type Instantaneo struct {
	// Metodo vai ao painel como consistency_method: "vss" quando há
	// instantâneo, "none" quando se tentou e não deu, vazio quando não se
	// tentou (outro sistema, plano que dispensa).
	Metodo string
	// Aviso é por que não houve instantâneo, quando se tentou.
	Aviso string

	// volumes leva a unidade ("C:") ao dispositivo do instantâneo dela
	// (`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy5`).
	volumes map[string]string
	soltar  func()
	uma     sync.Once
}

// criar monta o instantâneo das unidades e devolve o dispositivo de cada uma e
// a função que o solta. Os testes a trocam.
var criar = criarVSS

// noWindows diz se o instantâneo se aplica; os testes o trocam.
var noWindows = caminho.Windows

// Abrir cria o instantâneo das unidades em que estão as pastas do plano.
//
// Nunca falha: sem instantâneo, devolve um que lê do disco, com o motivo em
// Aviso. modo é o do plano (`auto`, `off`); vazio é `auto` — o painel de antes
// do campo não o manda.
func Abrir(ctx context.Context, modo string, pastas []string) *Instantaneo {
	if !noWindows || modo == ModoDesligado {
		return &Instantaneo{}
	}
	unidades := unidadesDoPlano(pastas)
	if len(unidades) == 0 {
		return &Instantaneo{}
	}
	cctx, cancelar := context.WithTimeout(ctx, PrazoDaCriacao)
	defer cancelar()
	inicio := time.Now()
	volumes, soltar, err := criar(cctx, unidades)
	if err != nil {
		slog.Warn("instantâneo do volume indisponível; os arquivos serão lidos direto do disco",
			"unidades", strings.Join(unidades, ","), "err", err)
		return &Instantaneo{
			Metodo: MetodoNenhum,
			Aviso:  "não consegui criar o instantâneo (VSS) de " + strings.Join(unidades, ", ") + ": " + err.Error(),
		}
	}
	slog.Info("instantâneo do volume criado", "unidades", strings.Join(unidades, ","),
		"duracao", time.Since(inicio).Round(time.Millisecond))
	i := &Instantaneo{Metodo: MetodoVSS, volumes: volumes, soltar: soltar}
	// A unidade que não aceita instantâneo (um pendrive em exFAT) fica de
	// fora do mapa e é lida do disco; as outras seguem congeladas.
	var fora []string
	for _, u := range unidades {
		if _, ok := volumes[u]; !ok {
			fora = append(fora, u)
		}
	}
	if len(fora) > 0 {
		i.Aviso = strings.Join(fora, ", ") + " não aceita instantâneo (VSS): FAT32, exFAT, unidade de rede ou subst"
	}
	return i
}

// Ler leva o caminho real de um arquivo (`C:\Users\x`) ao mesmo arquivo dentro
// do instantâneo. Sem instantâneo daquela unidade, devolve o mesmo caminho.
//
// A chave no bucket continua sendo a do disco: quem restaura vê C:\Users\x, e
// não o dispositivo da cópia de sombra.
func (i *Instantaneo) Ler(p string) string {
	if i == nil {
		return p
	}
	return lerEm(i.volumes, p)
}

// Ativo diz se a leitura é de um instantâneo.
func (i *Instantaneo) Ativo() bool { return i != nil && len(i.volumes) > 0 }

// Fechar solta o instantâneo. Pode ser chamada mais de uma vez.
func (i *Instantaneo) Fechar() {
	if i == nil || i.soltar == nil {
		return
	}
	i.uma.Do(i.soltar)
}

// lerEm troca a unidade pelo dispositivo do instantâneo dela.
//
// `C:\` vira `<dispositivo>\`, com a barra: sem ela, o caminho abre o
// dispositivo, e não a pasta raiz do volume.
func lerEm(volumes map[string]string, p string) string {
	u := caminho.Unidade(p)
	if u == "" {
		return p
	}
	dispositivo, ok := volumes[u]
	if !ok {
		return p
	}
	resto := p[len(u):]
	if resto == "" {
		resto = `\`
	}
	return dispositivo + resto
}

// unidadesDoPlano são as unidades das pastas do plano, sem repetir, em ordem.
func unidadesDoPlano(pastas []string) []string {
	vistas := map[string]bool{}
	var unidades []string
	for _, p := range pastas {
		u := caminho.Unidade(p)
		if u == "" && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)) && !strings.HasPrefix(p, `\\`) {
			// Plano antigo, escrito à moda Linux: é lido no disco do sistema
			// (caminho.NoDisco).
			u = "C:"
		}
		if u == "" || vistas[u] {
			continue
		}
		vistas[u] = true
		unidades = append(unidades, u)
	}
	sort.Strings(unidades)
	return unidades
}
