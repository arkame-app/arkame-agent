package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/zimbra"
)

// nomeDaPastaDeRestauracao: o painel restaura a conta que vai ser importada em
// <pasta da exportação>/../zimbra-restauracao/<job>/<conta>
// (`restauracao-do-zimbra.ts`): ao lado da exportação, no mesmo disco que o
// cliente escolheu para ela (antes era fixa em /var/backups, no disco do
// sistema; Hugo, 17/10). O agente só importa de dentro de uma pasta com este
// nome, que seja dele (do root) e fechada para os outros: é ela que o
// RemoveAll do fim limpa.
const nomeDaPastaDeRestauracao = "zimbra-restauracao"

// tipoDaLimpeza: apagar a pasta da restauração que não terminou inteira (0.5.5).
const tipoDaLimpeza = "zimbra_cleanup"

// errSemPasta: a pasta da restauração não está no disco.
var errSemPasta = errors.New("pasta ausente")

// posAcao: o que fazer depois de uma restauração concluída, como o painel manda
// em GET /api/agents/<id>/post-actions.
type posAcao struct {
	JobID          string `json:"job_id"`
	Type           string `json:"type"`
	Path           string `json:"path"`
	Account        string `json:"account"`
	To             string `json:"to,omitempty"`
	Settings       bool   `json:"settings"`
	WithForwarding bool   `json:"with_forwarding"`
}

// intervaloDasPosAcoes: a importação espera os arquivos chegarem; uma consulta
// por minuto basta, e não pesa no painel.
const intervaloDasPosAcoes = time.Minute

// posAcaoLoop consulta as pós-ações pendentes deste servidor e as executa,
// uma por vez — hoje, importar no Zimbra a conta que a tela do plano mandou
// restaurar (antes era a linha de comando no servidor; fundador, 17/10). A
// pendência é do painel: o agente que reinicia no meio volta a encontrá-la.
//
// Só no processo principal de um servidor Linux com o Zimbra, e ele pede as de
// todos os armazenamentos deste servidor: a restauração de um plano que grava na
// cópia local é feita pelo processo da cópia, mas os arquivos caem no mesmo
// disco, e quem importa é o principal.
func posAcaoLoop(ctx context.Context, c *api.Client, cfg *config.Config, oc *ocupacao) {
	if runtime.GOOS != "linux" || cfg.LocalStoreProgram != "" || !temZimbra() {
		return
	}
	ticker := time.NewTicker(intervaloDasPosAcoes)
	defer ticker.Stop()
	for {
		processarPosAcoes(ctx, c, cfg, oc, importarContaDoZimbra)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// importador é a importação em si; variável nos testes.
type importador func(ctx context.Context, a posAcao, log *bytes.Buffer) error

func processarPosAcoes(ctx context.Context, c *api.Client, cfg *config.Config, oc *ocupacao, importar importador) {
	var resp struct {
		Actions []posAcao `json:"actions"`
	}
	if err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/post-actions", &resp); err != nil {
		slog.Debug("pós-ações: consulta falhou", "err", err)
		return
	}
	for _, a := range resp.Actions {
		if ctx.Err() != nil {
			return
		}
		// A troca do programa espera a importação, como espera a restauração.
		if !oc.comecarRestauracao() {
			return
		}
		var log bytes.Buffer
		err := conferirPosAcao(a)
		if a.Type == tipoDaLimpeza {
			// A restauração não terminou inteira: só apaga a pasta temporária
			// (as conferências são as da importação).
			if err == nil {
				pasta := filepath.Dir(filepath.Clean(a.Path))
				if rerr := os.RemoveAll(pasta); rerr != nil {
					err = fmt.Errorf("apagando %s: %w", pasta, rerr)
				} else {
					fmt.Fprintf(&log, "pasta temporária apagada: %s\n", pasta)
				}
			} else if errors.Is(err, errSemPasta) {
				err = nil // já não estava no disco
			}
		} else if err == nil {
			err = importar(ctx, a, &log)
			// Parado no meio (o serviço reiniciando): nem apaga nem relata. A
			// pendência fica no painel e o próximo processo importa de novo da
			// mesma pasta (a importação pula o que já entrou).
			if ctx.Err() != nil {
				oc.terminarRestauracao()
				return
			}
			// A cópia da caixa não fica no disco, importada ou não: o
			// resultado é gravado uma vez só, e uma nova tentativa é um novo
			// pedido de restauração, com a sua própria pasta. Só depois da
			// conferência, que garante a pasta dentro das restaurações.
			pasta := filepath.Dir(filepath.Clean(a.Path))
			if rerr := os.RemoveAll(pasta); rerr != nil {
				slog.Warn("não consegui apagar a pasta da restauração", "pasta", pasta, "err", rerr)
			}
		}
		oc.terminarRestauracao()
		corpo := map[string]any{"ok": err == nil, "log": ultimasLinhas(log.String(), 4000)}
		if err != nil {
			corpo["error"] = err.Error()
			slog.Warn("pós-ação do Zimbra falhou", "tipo", a.Type, "job_id", a.JobID, "conta", a.Account, "err", err)
		} else if a.Type == tipoDaLimpeza {
			slog.Info("pasta da restauração do Zimbra que não terminou apagada", "job_id", a.JobID, "conta", a.Account)
		} else {
			slog.Info("conta do Zimbra importada", "job_id", a.JobID, "conta", a.Account)
		}
		if perr := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/post-actions/"+url.PathEscape(a.JobID), corpo, nil); perr != nil {
			slog.Warn("pós-ação: não consegui relatar o resultado", "job_id", a.JobID, "err", perr)
		}
	}
}

// conferirPosAcao recusa o que não for uma importação do Zimbra de dentro da
// pasta das restaurações: o painel não manda o agente ler de qualquer lugar.
func conferirPosAcao(a posAcao) error {
	if a.Type != "zimbra_restore" && a.Type != tipoDaLimpeza {
		return fmt.Errorf("pós-ação desconhecida: %q", a.Type)
	}
	if !zimbra.ContaValida(a.Account) || (a.To != "" && !zimbra.ContaValida(a.To)) {
		return fmt.Errorf("conta inválida")
	}
	p := filepath.Clean(a.Path)
	base := filepath.Dir(filepath.Dir(p))
	if !filepath.IsAbs(p) || filepath.Base(p) != a.Account || filepath.Base(base) != nomeDaPastaDeRestauracao || filepath.Dir(p) == base {
		return fmt.Errorf("pasta fora das restaurações do Zimbra: %s", a.Path)
	}
	if err := pastaFechada(base); err != nil {
		return fmt.Errorf("a pasta das restaurações do Zimbra %s não é segura: %w", base, err)
	}
	st, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("a pasta restaurada não está no servidor: %w", errors.Join(errSemPasta, err))
	}
	if !st.IsDir() {
		return fmt.Errorf("%s não é uma pasta", p)
	}
	return nil
}

// importarContaDoZimbra chama a mesma restauração da linha de comando.
func importarContaDoZimbra(ctx context.Context, a posAcao, log *bytes.Buffer) error {
	cli, err := zimbra.Conectar(ctx, "")
	if err != nil {
		return err
	}
	return zimbra.Restaurar(ctx, cli, zimbra.OpcoesDeRestauracao{
		De:              filepath.Clean(a.Path),
		Para:            a.To,
		Resolver:        "skip",
		Configuracao:    a.Settings,
		Encaminhamentos: a.WithForwarding,
	}, log)
}

// ultimasLinhas: o fim do log, que é onde está o erro.
func ultimasLinhas(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}
