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

// `outros` são mais arquivos a vigiar: no Docker, o programa do volume e o
// registro da troca (o da imagem não muda; o irmão sobe o do volume quando o
// principal o confirma).
func seguirOPrograma(ctx context.Context, oc *ocupacao, reiniciar func(), outros ...string) {
	programa, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(programa); err == nil {
		programa = real
	}
	arquivos := append([]string{programa}, outros...)
	// A marca de cada arquivo: data e tamanho; o que não existe conta como vazio.
	marca := func() string {
		m := ""
		for _, a := range arquivos {
			if fi, err := os.Stat(a); err == nil {
				m += fi.ModTime().String() + "|" + strconv.FormatInt(fi.Size(), 10) + ";"
			} else {
				m += "-;"
			}
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
