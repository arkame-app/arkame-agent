package daemon

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/enrollment"
)

// Renovação do token.
//
// O JWT do agente vence (hoje, um ano depois da aprovação). Sem renovação, o
// agente parava de falar com o painel no dia seguinte e só uma reinstalação o
// trazia de volta. O painel manda um token novo na resposta do heartbeat
// quando o atual vence em menos de 90 dias; o agente grava (troca atômica,
// arquivo protegido como o original) e passa a usá-lo na hora.

// avisoDeVencimento: a partir de quanto falta o agente avisa no log que o
// token vai vencer e o painel ainda não mandou outro.
const avisoDeVencimento = 30 * 24 * time.Hour

// intervaloDoAviso evita repetir o aviso a cada heartbeat.
const intervaloDoAviso = 24 * time.Hour

// estadoDoToken acompanha a renovação entre heartbeats.
type estadoDoToken struct {
	ultimoAviso time.Time
	agora       func() time.Time
}

// tratarRespostaDoHeartbeat aplica o token novo, se veio, ou avisa do
// vencimento próximo.
func (e *estadoDoToken) tratarRespostaDoHeartbeat(c *api.Client, cfg *config.Config, resp api.HeartbeatResponse) {
	agora := time.Now
	if e.agora != nil {
		agora = e.agora
	}
	novo := strings.TrimSpace(resp.NewToken)
	if novo != "" && novo != c.Bearer() {
		if strings.ContainsAny(novo, " \t\r\n") || strings.Count(novo, ".") != 2 {
			slog.Error("o painel mandou um token novo malformado; mantendo o atual")
		} else {
			// Troca na memória mesmo se a gravação falhar: o painel já emitiu
			// o novo, e o antigo pode deixar de valer. A falha fica no log —
			// num reinício, o agente voltaria ao token do disco.
			c.SetBearer(novo)
			if err := enrollment.PersistToken(cfg, novo); err != nil {
				slog.Error("token renovado em uso, mas não consegui gravá-lo; num reinício o agente volta ao token antigo",
					"path", cfg.TokenPath, "err", err)
			} else {
				venc, _ := vencimentoDoJWT(novo)
				slog.Info("token do agente renovado pelo painel", "path", cfg.TokenPath, "vence_em", venc)
			}
			return
		}
	}

	venc, ok := vencimentoDoJWT(c.Bearer())
	if !ok || venc.Sub(agora()) > avisoDeVencimento {
		return
	}
	if !e.ultimoAviso.IsZero() && agora().Sub(e.ultimoAviso) < intervaloDoAviso {
		return
	}
	e.ultimoAviso = agora()
	slog.Warn("o token do agente vence em breve e o painel ainda não mandou um novo; sem renovação, o agente para de falar com o painel no vencimento",
		"vence_em", venc, "faltam_dias", int(venc.Sub(agora()).Hours()/24))
}

// vencimentoDoJWT lê o exp do payload do JWT, sem verificar a assinatura (o
// agente não tem o segredo; aqui só importa a data).
func vencimentoDoJWT(token string) (time.Time, bool) {
	partes := strings.Split(token, ".")
	if len(partes) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(partes[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	f, err := claims.Exp.Float64()
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}
