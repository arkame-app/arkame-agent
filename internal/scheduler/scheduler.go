// Package scheduler avalia se um plano deve executar agora, considerando
// schedule (cron, daily, hourly) + janelas de tempo permitidas + estado de pause.
package scheduler

import (
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// ShouldRun decide se um plano está elegível para execução em `now`.
// Regras:
//  1. Plano precisa estar active (caller responsável por filtrar)
//  2. next_run_at <= now (ou manual, que responde a comando)
//  3. now cai dentro de alguma janela de tempo (ou sem janelas = sempre permitido)
func ShouldRun(plan api.Plan, now time.Time) bool {
	if plan.Schedule.Type == "manual" {
		// Manual só executa via trigger explícito do painel: next_run_at setado
		// (botão "Executar agora") e já vencido. O painel limpa next_run_at ao
		// iniciar a sessão, tornando o disparo one-shot.
		return plan.NextRunAt != nil && !now.Before(*plan.NextRunAt)
	}
	if plan.NextRunAt != nil && now.Before(*plan.NextRunAt) {
		return false
	}
	if len(plan.Windows) > 0 && !InAnyWindow(plan.Windows, now) {
		return false
	}
	return true
}

// InAnyWindow retorna true se `now` cai dentro de alguma das janelas.
// Uma janela é identificada pelo weekday + horário. Suporta janelas que
// atravessam meia-noite (start 22:00, end 06:00).
func InAnyWindow(windows []api.Window, now time.Time) bool {
	weekday := strings.ToLower(now.Weekday().String()[:3]) // "mon", "tue", ...
	currentMin := now.Hour()*60 + now.Minute()

	for _, w := range windows {
		if w.AllDay && containsDay(w.Days, weekday) {
			return true
		}
		if !containsDay(w.Days, weekday) {
			continue
		}
		startMin, ok := parseHHMM(w.Start)
		if !ok {
			continue
		}
		endMin, ok := parseHHMM(w.End)
		if !ok {
			continue
		}
		if startMin <= endMin {
			// Janela normal (ex: 08:00 → 18:00)
			if currentMin >= startMin && currentMin < endMin {
				return true
			}
		} else {
			// Janela cruzando meia-noite (ex: 22:00 → 06:00)
			if currentMin >= startMin || currentMin < endMin {
				return true
			}
		}
	}
	return false
}

func containsDay(days []string, want string) bool {
	for _, d := range days {
		if strings.EqualFold(d, want) {
			return true
		}
	}
	return false
}

func parseHHMM(s string) (int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := atoi(parts[0])
	m, err2 := atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errInvalid
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errInvalid = &invalidErr{}

type invalidErr struct{}

func (*invalidErr) Error() string { return "invalid integer" }
