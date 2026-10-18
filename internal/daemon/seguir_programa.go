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

// `versaoNoVolume`, se dada (Docker), é a marca inteira: a versão confirmada
// no volume. O executável não serve de marca lá: o programa da imagem não
// muda, e o do volume é trocado no começo da prova. E o "" da prova (nada
// confirmado) não conta como mudança: o irmão segue com o que tem e só
// reinicia quando o principal confirma uma versão diferente (revisão, 18/10:
// da 2ª atualização em diante ele caía no programa da imagem durante a prova).
func seguirOPrograma(ctx context.Context, oc *ocupacao, reiniciar func(), versaoNoVolume func() string) {
	programa, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(programa); err == nil {
		programa = real
	}
	marca := func() string {
		if versaoNoVolume != nil {
			return versaoNoVolume()
		}
		if fi, err := os.Stat(programa); err == nil {
			return fi.ModTime().String() + "|" + strconv.FormatInt(fi.Size(), 10)
		}
		return "-"
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
		if m := marca(); m == m0 || versaoNoVolume != nil && m == "" {
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
