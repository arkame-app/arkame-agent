package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// O processo da cópia local roda o mesmo programa do principal, mas não se
// atualiza sozinho (ARKAME_AUTO_UPDATE=off): quem troca o arquivo é o
// principal. Sem isto, o processo local seguia na versão antiga até alguém o
// reiniciar. seguirOPrograma confere o executável e, quando ele muda, sai do
// jeito da atualização (reiniciar → ErrReiniciar), para o gerenciador de
// serviço subir o programa novo — com a ocupação reservada antes, para não
// cortar um backup ou uma restauração no meio.
var intervaloDeSeguir = time.Minute

func seguirOPrograma(ctx context.Context, oc *ocupacao, reiniciar func()) {
	programa, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(programa); err == nil {
		programa = real
	}
	marca := func() (time.Time, int64, bool) {
		fi, err := os.Stat(programa)
		if err != nil {
			return time.Time{}, 0, false
		}
		return fi.ModTime(), fi.Size(), true
	}
	t0, n0, ok := marca()
	if !ok {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(intervaloDeSeguir):
		}
		t, n, ok := marca()
		if !ok || (t.Equal(t0) && n == n0) {
			continue
		}
		if _, motivo := oc.reservar(); motivo != "" {
			slog.Info("o programa do agente mudou; a cópia local reinicia quando terminar o que está fazendo", "motivo", motivo)
			continue
		}
		slog.Info("o programa do agente mudou (o principal o atualizou); a cópia local reinicia com ele", "programa", programa)
		reiniciar()
		return
	}
}
