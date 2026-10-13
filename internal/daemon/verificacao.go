package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	gosync "sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/arkame-app/agent/internal/verificacao"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// O teste de restauração por amostra (contrato, itens 34 a 38): o painel
// escolhe versões do catálogo, o agente as restaura para uma pasta privada
// dentro da pasta do estado, confere, apaga e relata.
//
// Não cruza com backup nem com a limpeza de retenção: pega a mesma trava do
// bucket, sem esperar (verificacaoDoBucket). A limpeza que rodasse no meio
// apagaria uma versão da amostra, e o teste acusaria um backup são.

// Os tempos do laço; variáveis para os testes.
var (
	// intervaloDaVerificacao é o intervalo entre perguntas ao painel, que
	// quase sempre responde "nada": ele emite uma rodada por semana.
	intervaloDaVerificacao = 1 * time.Hour
	// primeiraVerificacao: a primeira pergunta não sai no arranque — é quando
	// o backup atrasado e a limpeza disputam o bucket.
	primeiraVerificacao = 5 * time.Minute
	// reprovaDaVerificacao: bucket ocupado, tenta de novo.
	reprovaDaVerificacao = 1 * time.Minute
	// esperaSemRotaDeVerificacao: o painel não tem a rota (404): é um painel
	// de antes do teste de restauração.
	esperaSemRotaDeVerificacao = 24 * time.Hour
	// prazoDoPlanoDeVerificacao: o painel monta a amostra na hora, sobre o
	// catálogo; com milhões de arquivos passa dos 30 s do client.
	prazoDoPlanoDeVerificacao = 5 * time.Minute
)

// pastaDaVerificacao é o nome, dentro da pasta do estado, da pasta onde as
// amostras são restauradas.
const pastaDaVerificacao = "verificacao"

var formaDeUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// podeVerificar diz se este processo tem como fazer o teste de restauração:
// um armazenamento configurado e pasta de estado onde restaurar a amostra.
func podeVerificar(cfg *config.Config) bool {
	return cfg != nil && cfg.AgentID != "" && cfg.StorageID != "" && cfg.StorageBucket != "" && cfg.StateDir != ""
}

// pastaDaVerificacaoEm é, dentro de uma pasta do estado, a pasta das rodadas
// DESTE agente + armazenamento. Os processos irmãos (outros buckets do mesmo
// agente) usam a mesma pasta do estado, cada um com a sua.
func pastaDaVerificacaoEm(cfg *config.Config, dirDoEstado string) string {
	base := filepath.Base(arquivoDoEstado(cfg, dirDoEstado))
	return filepath.Join(dirDoEstado, pastaDaVerificacao, base[:len(base)-len(filepath.Ext(base))])
}

// pastaComum diz se p existe e é uma pasta de verdade (não link).
func pastaComum(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
}

// limparVerificacoes apaga o que rodadas anteriores deste agente +
// armazenamento deixaram no disco (o serviço parou no meio de uma). Só a pasta
// deste processo: a de um irmão pode estar em uso. Roda no arranque do daemon.
func limparVerificacoes(cfg *config.Config) {
	if !podeVerificar(cfg) {
		return
	}
	for _, dir := range pastasDoEstado(cfg) {
		raiz := filepath.Join(dir, pastaDaVerificacao)
		// Só numa pasta do estado que é mesmo do agente, e sem seguir link:
		// o RemoveAll roda como root.
		if !pastaComum(dir) || conferirDono(dir) != nil || !pastaComum(raiz) {
			continue
		}
		minha := pastaDaVerificacaoEm(cfg, dir)
		if pastaComum(minha) {
			if err := os.RemoveAll(minha); err != nil {
				slog.Warn("não consegui limpar a pasta do teste de restauração", "pasta", minha, "err", err)
			} else {
				slog.Info("restos de um teste de restauração interrompido foram apagados", "pasta", minha)
			}
		}
		_ = os.Remove(raiz) // só sai se ficou vazia
	}
}

// abrirPastaDaRodada cria, vazia e privada, a pasta da rodada na primeira
// pasta do estado que aceitar. O run_id já foi conferido (formaDeUUID).
func abrirPastaDaRodada(cfg *config.Config, runID string) (string, error) {
	var ultimo error = errors.New("sem pasta de estado")
	for _, dir := range pastasDoEstado(cfg) {
		rodada := filepath.Join(pastaDaVerificacaoEm(cfg, dir), runID)
		// A rodada reentregue recomeça do zero: o que a tentativa anterior
		// deixou não vale como item restaurado.
		if pastaComum(filepath.Dir(rodada)) {
			if err := os.RemoveAll(rodada); err != nil {
				ultimo = err
				continue
			}
		}
		if err := pastaPrivada(rodada); err != nil {
			ultimo = err
			continue
		}
		// Gravável? A pasta pode existir num disco só de leitura.
		f, err := os.CreateTemp(rodada, ".teste-*")
		if err != nil {
			ultimo = err
			_ = os.Remove(rodada)
			continue
		}
		f.Close()
		_ = os.Remove(f.Name())
		return rodada, nil
	}
	return "", ultimo
}

// fecharPastaDaRodada apaga a pasta da rodada e, se ficaram vazias, as de cima.
func fecharPastaDaRodada(rodada string) {
	if err := os.RemoveAll(rodada); err != nil {
		slog.Warn("não consegui apagar a pasta do teste de restauração", "pasta", rodada, "err", err)
	}
	minha := filepath.Dir(rodada)
	_ = os.Remove(minha)
	_ = os.Remove(filepath.Dir(minha))
}

// PastasDeVerificacao são as pastas de teste de restauração que ESTE AGENTE
// (de qualquer armazenamento) pode ter deixado no disco, nas pastas dadas. É o
// que o `uninstall` apaga junto com o estado.
func PastasDeVerificacao(cfg *config.Config, pastasDoEstadoLocal []string) []string {
	if cfg.AgentID == "" {
		return nil
	}
	var out []string
	for _, dir := range pastasDoEstadoLocal {
		raiz := filepath.Join(dir, pastaDaVerificacao)
		if !pastaComum(raiz) {
			continue
		}
		achadas, _ := filepath.Glob(filepath.Join(raiz, estado.PrefixoDoAgente(cfg.AgentID)+"*"))
		for _, p := range achadas {
			if pastaComum(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// verifyLoop é o laço do teste de restauração por amostra.
func verifyLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, trava *exclusaoDoBucket) {
	limparVerificacoes(cfg)
	if !podeVerificar(cfg) {
		return
	}
	espera := esperaDaVerificacao(cfg, primeiraVerificacao)
	for {
		timer := time.NewTimer(espera)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		rodou, proxima := rodadaDeVerificacao(ctx, c, s3c, cfg, trava)
		if rodou {
			espera = esperaDaVerificacao(cfg, proxima)
			continue
		}
		// Adiada. Avisa uma vez, não a cada minuto de espera.
		if espera != reprovaDaVerificacao {
			slog.Info("teste de restauração adiado: o bucket está ocupado (backup ou limpeza); tentando de novo quando terminar",
				"bucket", cfg.StorageBucket)
		}
		espera = reprovaDaVerificacao
	}
}

// esperaDaVerificacao aplica o intervalo configurado
// (ARKAME_RESTORE_TEST_INTERVAL_SEC) à espera padrão: ele só encurta — a
// primeira pergunta e o intervalo de hora em hora. A espera de um dia do painel
// sem a rota (404) não é encurtada: é o que mantém o agente quieto.
func esperaDaVerificacao(cfg *config.Config, padrao time.Duration) time.Duration {
	if cfg == nil || cfg.RestoreTestIntervalSec <= 0 || padrao == esperaSemRotaDeVerificacao {
		return padrao
	}
	if d := time.Duration(cfg.RestoreTestIntervalSec) * time.Second; d < padrao {
		return d
	}
	return padrao
}

// rodadaDeVerificacao faz uma passada com o bucket fechado para backup e
// limpeza. rodou=false: o bucket estava ocupado e nada foi perguntado ao
// painel. proxima é quanto esperar até a passada seguinte.
func rodadaDeVerificacao(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, trava *exclusaoDoBucket) (rodou bool, proxima time.Duration) {
	proxima = intervaloDaVerificacao
	if !podeVerificar(cfg) {
		return true, proxima
	}
	rodou = trava.verificacao(func() { proxima = verificar(ctx, c, s3c, cfg, trava.backupEsperando) })
	return rodou, proxima
}

// semRotaDeVerificacao garante uma linha de log por processo quando o painel
// não tem a rota.
var semRotaDeVerificacao gosync.Once

// verificar pergunta ao painel se há amostra a restaurar e a restaura. Quem
// chama segura a exclusaoDoBucket. Devolve quanto esperar até a próxima
// pergunta.
func verificar(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, ceder func() bool) time.Duration {
	var plano api.VerifyPlanResponse
	path := "/api/agents/" + cfg.AgentID + "/verify-plan?storage_id=" + url.QueryEscape(cfg.StorageID)
	if err := c.ComPrazo(prazoDoPlanoDeVerificacao).GET(ctx, path, &plano); err != nil {
		var he *api.HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			semRotaDeVerificacao.Do(func() {
				slog.Info("o painel ainda não tem o teste de restauração por amostra; pergunto de novo em um dia")
			})
			return esperaSemRotaDeVerificacao
		}
		slog.Debug("poll verify-plan falhou", "err", err)
		return intervaloDaVerificacao
	}
	if plano.RunID == "" || len(plano.Items) == 0 {
		return intervaloDaVerificacao
	}
	if (plano.Bucket != "" && plano.Bucket != cfg.StorageBucket) || (plano.StorageID != "" && plano.StorageID != cfg.StorageID) {
		// De um bucket que este processo não atende. Silêncio: o processo
		// irmão com as credenciais certas pega.
		slog.Debug("plano de verificação de outro armazenamento", "plano", plano.Bucket, "meu", cfg.StorageBucket)
		return intervaloDaVerificacao
	}
	if !formaDeUUID.MatchString(plano.RunID) {
		// O run_id vira nome de pasta: só entra o que tem a forma esperada.
		slog.Warn("plano de verificação recusado: run_id fora do formato")
		return intervaloDaVerificacao
	}
	itens := plano.Items
	if len(itens) > verificacao.MaxItens {
		itens = itens[:verificacao.MaxItens]
	}
	maxBytes, prazo := verificacao.Limites(plano.MaxBytes, plano.DeadlineSeconds)

	slog.Info("teste de restauração por amostra pedido pelo painel", "run_id", plano.RunID, "itens", len(itens))
	inicio := time.Now().UTC()

	var resultados []api.VerifyResultItem
	rodada, err := abrirPastaDaRodada(cfg, plano.RunID)
	if err != nil {
		// Sem onde gravar não há o que testar — e não é defeito do backup.
		slog.Warn("teste de restauração sem pasta onde restaurar a amostra", "err", err)
		for _, it := range itens {
			resultados = append(resultados, api.VerifyResultItem{Key: it.Key, VersionID: it.VersionID,
				Status: verificacao.StatusPulado, Error: verificacao.PuladoErroLocal, Detail: "sem pasta de verificação"})
		}
	} else {
		defer fecharPastaDaRodada(rodada)
		var interrompida bool
		resultados, interrompida = verificacao.Rodar(ctx, verificacao.Opcoes{
			S3:         s3c,
			Bucket:     cfg.StorageBucket,
			PrefixRoot: plano.PrefixRoot,
			Pasta:      rodada,
			HostRoot:   cfg.HostRoot,
			MaxBytes:   maxBytes,
			Prazo:      prazo,
			Ceder:      ceder,
		}, itens)
		if interrompida {
			// O serviço está parando: sem relato a rodada segue emitida e o
			// painel a reentrega. A pasta sai no defer (e, se o processo
			// morrer antes, no arranque seguinte).
			slog.Info("teste de restauração interrompido pela parada do serviço; a rodada será refeita", "run_id", plano.RunID)
			return intervaloDaVerificacao
		}
	}

	// O estado local do backup não pode seguir afirmando a versão que não
	// está no bucket, ou que está lá com outro conteúdo: o próximo backup lê
	// o arquivo e o envia de novo.
	var ok, falhas, pulados int
	var baixas []estado.VersaoApagada
	for _, r := range resultados {
		switch r.Status {
		case verificacao.StatusOK:
			ok++
		case verificacao.StatusFalhou:
			falhas++
			switch r.Error {
			case verificacao.ErroNaoEncontrado, verificacao.ErroHashDiverge, verificacao.ErroTamanhoDiverge:
				baixas = append(baixas, estado.VersaoApagada{Chave: r.Key, Versao: r.VersionID})
			}
		default:
			pulados++
		}
	}
	darBaixaNoEstado(cfg, baixas)

	relato := api.VerifyResult{RunID: plano.RunID, StartedAt: inicio, FinishedAt: time.Now().UTC(), Items: resultados}
	// O relato sai mesmo com o serviço parando, como o do expurgo: a rodada
	// terminou, e refazê-la baixaria tudo de novo.
	fctx, fcancel := contextoDeFinalizacao(ctx)
	defer fcancel()
	var resposta api.VerifyResultResponse
	if err := c.POST(fctx, "/api/agents/"+cfg.AgentID+"/verify-result", relato, &resposta); err != nil {
		slog.Warn("relato do teste de restauração falhou; o painel reentrega a rodada", "run_id", plano.RunID, "err", err)
		return intervaloDaVerificacao
	}
	nivel := slog.LevelInfo
	if falhas > 0 {
		nivel = slog.LevelWarn
	}
	slog.Log(ctx, nivel, "teste de restauração concluído",
		"run_id", plano.RunID, "ok", ok, "falhas", falhas, "pulados", pulados,
		"status_no_painel", resposta.Status, "recusados_pelo_painel", resposta.Rejected)
	return intervaloDaVerificacao
}
