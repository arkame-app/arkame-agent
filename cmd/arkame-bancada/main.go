// arkame-bancada é a ferramenta de medição de escala do agente: gera árvores
// sintéticas determinísticas, aplica mutações e mede um programa do agente
// (este ou um release antigo) contra um S3 falso e um painel falso, contando
// tempo, memória, requisições por operação e os lotes do índice.
//
// Não faz parte do release do agente. Uso típico:
//
//	arkame-bancada gerar --raiz /tmp/arvore --arquivos 100000 --semente 42
//	arkame-bancada medir --agente ./bin/arkame-agent --raiz /tmp/arvore \
//	    --casos primeira,sem-mudanca,alterar:0.001 --saida depois.json
//	arkame-bancada mutar --raiz /tmp/arvore --mutacao mudar-sem-tocar --quanto 0.001
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/spf13/cobra"
)

func main() {
	raiz := &cobra.Command{
		Use:           "arkame-bancada",
		Short:         "Bancada de escala do agente Arkame (árvores sintéticas, mutações e medição)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	raiz.AddCommand(cmdGerar(), cmdMutar(), cmdMedir())
	if err := raiz.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func cmdGerar() *cobra.Command {
	var a bancada.Arvore
	cmd := &cobra.Command{
		Use:   "gerar",
		Short: "Gera uma árvore sintética determinística",
		RunE: func(*cobra.Command, []string) error {
			inicio := time.Now()
			total, err := a.Gerar()
			if err != nil {
				return err
			}
			fmt.Printf("%d arquivos, %.1f MB em %s (%.1fs)\n", a.Arquivos, float64(total)/1e6, a.Raiz, time.Since(inicio).Seconds())
			return nil
		},
	}
	cmd.Flags().StringVar(&a.Raiz, "raiz", "", "pasta onde a árvore é criada")
	cmd.Flags().IntVar(&a.Arquivos, "arquivos", 100000, "quantos arquivos")
	cmd.Flags().Uint64Var(&a.Semente, "semente", 42, "semente (mesma semente, mesma árvore)")
	cmd.Flags().Float64Var(&a.Mediana, "mediana", 4096, "mediana do tamanho dos arquivos, em bytes (lognormal)")
	cmd.Flags().Float64Var(&a.Sigma, "sigma", 1.2, "sigma da lognormal")
	cmd.Flags().Int64Var(&a.Teto, "teto", 1<<20, "maior arquivo, em bytes")
	_ = cmd.MarkFlagRequired("raiz")
	return cmd
}

func cmdMutar() *cobra.Command {
	var raiz, mutacao string
	var quanto float64
	var semente uint64
	cmd := &cobra.Command{
		Use:   "mutar",
		Short: "Aplica uma mutação à árvore",
		Long: `Mutações: alterar, adicionar, remover, renomear-pasta, tocar-sem-mudar,
mudar-sem-tocar. --quanto é a fração dos arquivos (0–1), ou a quantidade em
"adicionar"; "renomear-pasta" o ignora.`,
		RunE: func(*cobra.Command, []string) error {
			n, err := bancada.Mutar(raiz, mutacao, quanto, semente)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %d arquivos\n", mutacao, n)
			return nil
		},
	}
	cmd.Flags().StringVar(&raiz, "raiz", "", "raiz da árvore")
	cmd.Flags().StringVar(&mutacao, "mutacao", "", "qual mutação")
	cmd.Flags().Float64Var(&quanto, "quanto", 0.001, "fração (0–1) ou quantidade")
	cmd.Flags().Uint64Var(&semente, "semente", 101, "semente da escolha dos arquivos")
	_ = cmd.MarkFlagRequired("raiz")
	_ = cmd.MarkFlagRequired("mutacao")
	return cmd
}

// Caso é o resultado medido de uma execução do agente.
type Caso struct {
	Caso        string           `json:"caso"`
	Mutacao     string           `json:"mutacao,omitempty"`
	Mutados     int              `json:"mutados,omitempty"`
	Sessao      string           `json:"sessao"`
	Indice      string           `json:"indice"` // inline | full | delta
	Sessoes     int              `json:"sessoes"`
	TempoS      float64          `json:"tempo_s"`
	CPUS        float64          `json:"cpu_s"`
	RSSMB       float64          `json:"rss_mb"`
	S3          map[string]int64 `json:"s3"`
	S3Bytes     int64            `json:"s3_bytes_recebidos"`
	Lotes       int              `json:"lotes"`
	Entradas    int64            `json:"entradas"`
	MaiorLoteB  int              `json:"maior_lote_bytes"`
	LotesBytes  int64            `json:"lotes_bytes"`
	CompleteB   int64            `json:"complete_bytes"`
	MaiorPOSTB  int64            `json:"maior_post_bytes"`
	Presentes   int              `json:"presentes"`
	Estatistica map[string]any   `json:"stats,omitempty"`
}

func cmdMedir() *cobra.Command {
	var (
		agente, raiz, casos, painelModo, saida, estadoDir, rotulo string
		trabalhadores                                             int
		prazo                                                     time.Duration
		semCatalogo                                               bool
	)
	cmd := &cobra.Command{
		Use:   "medir",
		Short: "Mede um programa do agente contra o S3 falso e o painel falso",
		Long: `Sobe um S3 falso e um painel falso neste processo e executa o programa do
agente dado (um processo por caso, como o serviço reiniciado), em sequência:

  primeira            a árvore como está
  sem-mudanca         nada muda
  <mutação>:<quanto>  aplica a mutação antes (alterar:0.001, remover:0.01,
                      adicionar:100, tocar-sem-mudar:0.01, renomear-pasta:0…)
  cache-perdido       apaga a pasta do estado local antes

O bucket e o painel falsos vivem só enquanto o comando roda: a sequência
inteira é uma medição. --painel antigo mede contra o painel de antes do índice
em lotes (o /complete inline de produção).`,
		RunE: func(*cobra.Command, []string) error {
			return medir(agente, raiz, strings.Split(casos, ","), painelModo, saida, estadoDir, rotulo, trabalhadores, prazo, semCatalogo)
		},
	}
	cmd.Flags().StringVar(&agente, "agente", "", "programa do agente a medir")
	cmd.Flags().StringVar(&raiz, "raiz", "", "árvore a proteger")
	cmd.Flags().StringVar(&casos, "casos", "primeira,sem-mudanca,alterar:0.001", "sequência de casos")
	cmd.Flags().StringVar(&painelModo, "painel", "novo", `"novo" (índice em lotes e delta) ou "antigo" (inline, como a produção v2.76.0)`)
	cmd.Flags().StringVar(&saida, "saida", "", "grava o resultado em JSON neste arquivo")
	cmd.Flags().StringVar(&estadoDir, "estado", "", "pasta do estado local do agente (padrão: temporária)")
	cmd.Flags().StringVar(&rotulo, "rotulo", "", "nome desta medição no resultado")
	cmd.Flags().IntVar(&trabalhadores, "trabalhadores", 0, "UPLOAD_CONCURRENCY do agente (0: o padrão dele)")
	cmd.Flags().DurationVar(&prazo, "prazo", 2*time.Hour, "prazo de cada caso")
	cmd.Flags().BoolVar(&semCatalogo, "sem-catalogo", false, "o painel falso só conta as entradas (para árvores muito grandes)")
	_ = cmd.MarkFlagRequired("agente")
	_ = cmd.MarkFlagRequired("raiz")
	return cmd
}

func servir(h http.Handler) (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	return "http://" + l.Addr().String(), func() { _ = srv.Close() }, nil
}

func medir(agente, raiz string, casos []string, painelModo, saida, estadoDir, rotulo string, trabalhadores int, prazo time.Duration, semCatalogo bool) error {
	raiz, err := filepath.Abs(raiz)
	if err != nil {
		return err
	}
	agente, err = filepath.Abs(agente)
	if err != nil {
		return err
	}
	s3f := bancada.NovoS3Falso()
	painel := bancada.NovoPainelFalso()
	painel.SemLotes = painelModo == "antigo"
	painel.SemCatalogo = semCatalogo
	// A medição não é da recusa por tamanho: o painel antigo falso aceita o
	// índice inteiro, para o "antes" fechar a sessão.
	painel.MaxInlineArquivos, painel.MaxInlineBytes = 1<<31-1, 1<<31-1
	painel.Planos = []api.Plan{{
		ID: "plano-bancada", Kind: "backup", Status: "active",
		SourcePaths: []string{raiz},
		StorageRef:  api.StorageRef{ID: "st-bancada", Bucket: "bancada"},
		Schedule:    api.Schedule{Type: "manual"},
	}}
	urlS3, fecharS3, err := servir(s3f)
	if err != nil {
		return err
	}
	defer fecharS3()
	urlPainel, fecharPainel, err := servir(painel)
	if err != nil {
		return err
	}
	defer fecharPainel()

	trabalho, err := os.MkdirTemp("", "arkame-bancada-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(trabalho)
	if estadoDir == "" {
		estadoDir = filepath.Join(trabalho, "estado")
	}
	// O token é só um texto: o painel falso não confere. Nada aqui é segredo.
	if err := os.WriteFile(filepath.Join(trabalho, "token.jwt"), []byte("bancada\n"), 0o600); err != nil {
		return err
	}
	env := []string{
		"PANEL_URL=" + urlPainel,
		"AGENT_ID=ag-bancada",
		"TOKEN_PATH=" + filepath.Join(trabalho, "token.jwt"),
		"PRIVATE_KEY_PATH=" + filepath.Join(trabalho, "key.pem"),
		"AGENT_ID_PATH=" + filepath.Join(trabalho, "agent.id"),
		"HOST_ROOT=/",
		"STORAGE_ACCESS_KEY=bancada",
		"STORAGE_SECRET_KEY=bancada",
		"STORAGE_ENDPOINT=" + urlS3,
		"STORAGE_REGION=us-east-1",
		"STORAGE_BUCKET=bancada",
		"STORAGE_ID=st-bancada",
		"POLL_INTERVAL_SEC=1",
		"ARKAME_AUTO_UPDATE=off",
		"ARKAME_STATE_DIR=" + estadoDir,
	}
	if trabalhadores > 0 {
		env = append(env, "UPLOAD_CONCURRENCY="+strconv.Itoa(trabalhadores))
	}
	config := filepath.Join(trabalho, "agent.env")
	if err := os.WriteFile(config, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		return err
	}

	var resultados []Caso
	for i, nome := range casos {
		nome = strings.TrimSpace(nome)
		c := Caso{Caso: nome}
		switch {
		case nome == "primeira" || nome == "sem-mudanca":
		case nome == "cache-perdido":
			if err := os.RemoveAll(estadoDir); err != nil {
				return err
			}
		default:
			mut, quanto, _ := strings.Cut(nome, ":")
			q, _ := strconv.ParseFloat(quanto, 64)
			n, err := bancada.Mutar(raiz, mut, q, uint64(100+i))
			if err != nil {
				return fmt.Errorf("caso %s: %w", nome, err)
			}
			c.Mutacao, c.Mutados = mut, n
		}
		if err := medirCaso(&c, agente, config, s3f, painel, prazo); err != nil {
			return fmt.Errorf("caso %s: %w", nome, err)
		}
		resultados = append(resultados, c)
		imprimir(c)
	}
	if saida != "" {
		b, _ := json.MarshalIndent(map[string]any{"rotulo": rotulo, "agente": agente, "painel": painelModo, "raiz": raiz, "casos": resultados}, "", "  ")
		if err := os.WriteFile(saida, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// medirCaso executa o agente até a sessão do plano fechar e recolhe os números.
func medirCaso(c *Caso, agente, config string, s3f *bancada.S3Falso, painel *bancada.PainelFalso, prazo time.Duration) error {
	antes := len(painel.Sessoes())
	s3f.Zerar()
	painel.Zerar()
	log, err := os.CreateTemp("", "arkame-bancada-agente-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(log.Name())
	defer log.Close()

	cmd := exec.Command(agente, "run", "--config", config)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), "ARKAME_AUTO_UPDATE=off")
	painel.Disparar()
	inicio := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	morreu := make(chan error, 1)
	go func() { morreu <- cmd.Wait() }()

	limite := time.After(prazo)
	fechou := false
	for !fechou {
		select {
		case err := <-morreu:
			return fmt.Errorf("o agente saiu antes de fechar a sessão: %v\n%s", err, fimDoLog(log.Name()))
		case <-limite:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-morreu
			return fmt.Errorf("prazo de %s esgotado\n%s", prazo, fimDoLog(log.Name()))
		case <-time.After(20 * time.Millisecond):
			ss := painel.Sessoes()
			if len(ss) > antes && ss[len(ss)-1].Status != "running" {
				// Uma sessão recusada é refeita em outra: espera a última.
				if u := ss[len(ss)-1]; u.ErrorCode == "index_rejected" {
					continue
				}
				fechou = true
			}
		}
	}
	c.TempoS = time.Since(inicio).Seconds()
	// O pico de memória é lido do próprio processo, antes de ele sair: o
	// ru_maxrss que o wait devolve começa do tamanho DESTE processo no
	// instante do fork (o filho nasce no espaço de memória do pai), e o
	// bucket e o painel falsos moram aqui.
	c.RSSMB = picoDoProcesso(cmd.Process.Pid)
	// Pelo PID do processo que este comando criou.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-morreu:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-morreu
	}
	if c.RSSMB == 0 {
		c.RSSMB = rssEmMB(cmd.ProcessState.SysUsage())
	}
	c.CPUS = (cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()).Seconds()

	ss := painel.Sessoes()
	u := ss[len(ss)-1]
	c.Sessao, c.Indice, c.Sessoes, c.Presentes = u.Status, u.Modo, len(ss)-antes, u.Presentes
	if u.Status == "failed" {
		c.Sessao += " (" + u.ErrorCode + ")"
	}
	c.S3, c.S3Bytes = s3f.Requisicoes(), s3f.BytesRecebidos()
	for _, l := range painel.LotesRecebidos() {
		c.Lotes++
		c.Entradas += int64(l.Entradas)
		c.LotesBytes += int64(l.Bytes)
		c.MaiorLoteB = max(c.MaiorLoteB, l.Bytes)
	}
	for rota, r := range painel.Rotas() {
		c.MaiorPOSTB = max(c.MaiorPOSTB, r.MaiorCorpo)
		if rota == "complete" {
			c.CompleteB = r.MaiorCorpo
		}
	}
	if st, ok := u.Complete["stats"].(map[string]any); ok {
		c.Estatistica = st
	}
	return nil
}

// picoDoProcesso lê o pico de memória residente (VmHWM) de um processo, em
// MiB. Zero onde não há /proc.
func picoDoProcesso(pid int) float64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmHWM:" {
			kb, _ := strconv.ParseFloat(f[1], 64)
			return kb / 1024
		}
	}
	return 0
}

func fimDoLog(arquivo string) string {
	b, _ := os.ReadFile(arquivo)
	if len(b) > 3000 {
		b = b[len(b)-3000:]
	}
	return string(b)
}

func imprimir(c Caso) {
	ops := make([]string, 0, len(c.S3))
	for op, n := range c.S3 {
		ops = append(ops, fmt.Sprintf("%s=%d", op, n))
	}
	sort.Strings(ops)
	fmt.Printf("%-22s sessão=%s índice=%s tempo=%.1fs cpu=%.1fs rss=%.0fMB\n", c.Caso, c.Sessao, c.Indice, c.TempoS, c.CPUS, c.RSSMB)
	fmt.Printf("%-22s s3: %s\n", "", strings.Join(ops, " "))
	fmt.Printf("%-22s painel: lotes=%d entradas=%d maior_lote=%dB complete=%dB maior_post=%dB presentes=%d\n",
		"", c.Lotes, c.Entradas, c.MaiorLoteB, c.CompleteB, c.MaiorPOSTB, c.Presentes)
}
