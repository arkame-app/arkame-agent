package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

// `marcaExtra`, se dada, entra na marca: no Docker, a versão confirmada no
// volume (o programa da imagem não muda; o irmão sobe o do volume quando o
// principal confirma a versão, e não a cada mudança do registro durante a prova).
func seguirOPrograma(ctx context.Context, oc *ocupacao, reiniciar func(), marcaExtra func() string) {
	programa, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(programa); err == nil {
		programa = real
	}
	marca := func() string {
		m := "-"
		if fi, err := os.Stat(programa); err == nil {
			m = fi.ModTime().String() + "|" + strconv.FormatInt(fi.Size(), 10)
		}
		if marcaExtra != nil {
			m += ";" + marcaExtra()
		}
		return m
	}
	if _, err := os.Stat(programa); err != nil {
		return
	}
	m0 := marca()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(intervaloDeSeguir):
		}
		if marca() == m0 {
			continue
		}
		if _, motivo := oc.reservar(); motivo != "" {
			slog.Info("o programa do agente mudou; este processo reinicia quando terminar o que está fazendo", "motivo", motivo)
			continue
		}
		slog.Info("o programa do agente mudou (o principal o atualizou); este processo reinicia com ele", "programa", programa)
		reiniciar()
		return
	}
}
