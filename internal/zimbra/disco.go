package zimbra

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// espacoNoDisco é variável para os testes.
var espacoNoDisco = espacoNoDiscoDoSistema

// intervaloDaGuarda: de quanto em quanto a exportação olha o disco.
var intervaloDaGuarda = time.Second

// guardaDoDisco para a exportação antes de encher o disco do servidor de
// correio (Zimbra Adubos, 17/10: a pasta da exportação e a cópia local no
// mesmo `/` de 49 GB, com as caixas em 213 GB no /opt). A exportação usa no
// máximo metade do que estava livre acima da reserva — a outra metade é para
// o backup, que copia a exportação para a cópia local, muitas vezes no mesmo
// disco — e nunca deixa o livre abaixo da reserva. O que não coube fica para
// a próxima sessão, como no prazo vencido.
type guardaDoDisco struct {
	pasta         string
	livreNoComeco uint64
	reserva       uint64
	limite        uint64
	estourou      atomic.Bool
	livreNaParada atomic.Uint64
}

// reservaDoDisco: 10% do disco, e nunca menos de 5 GiB.
func reservaDoDisco(total uint64) uint64 {
	const minimo = 5 << 30
	if r := total / 10; r > minimo {
		return r
	}
	return minimo
}

// novaGuarda mede o disco da pasta. Sem medida (outro sistema, erro do
// statfs), devolve nil: a exportação segue sem guarda, como antes.
func novaGuarda(pasta string) (*guardaDoDisco, error) {
	livre, total, err := espacoNoDisco(pasta)
	if err != nil {
		return nil, nil
	}
	g := &guardaDoDisco{pasta: pasta, livreNoComeco: livre, reserva: reservaDoDisco(total)}
	if livre <= g.reserva {
		return nil, fmt.Errorf("sem espaço para exportar o Zimbra em %s: %s livres, e a exportação deixa pelo menos %s no disco. Libere espaço ou use --dir numa pasta de um disco com espaço (e ponha essa pasta no plano)",
			pasta, bytesLegiveis(livre), bytesLegiveis(g.reserva))
	}
	g.limite = (livre - g.reserva) / 2
	return g, nil
}

// vigiar cancela o contexto devolvido quando a exportação passa do limite.
func (g *guardaDoDisco) vigiar(ctx context.Context) (context.Context, context.CancelFunc) {
	trabalho, cancelar := context.WithCancel(ctx)
	if g == nil {
		return trabalho, cancelar
	}
	go func() {
		t := time.NewTicker(intervaloDaGuarda)
		defer t.Stop()
		for {
			select {
			case <-trabalho.Done():
				return
			case <-t.C:
			}
			livre, _, err := espacoNoDisco(g.pasta)
			if err != nil {
				continue
			}
			var usado uint64
			if livre < g.livreNoComeco {
				usado = g.livreNoComeco - livre
			}
			if usado >= g.limite || livre <= g.reserva {
				g.livreNaParada.Store(livre)
				g.estourou.Store(true)
				cancelar()
				return
			}
		}
	}()
	return trabalho, cancelar
}

func (g *guardaDoDisco) parou() bool { return g != nil && g.estourou.Load() }

func bytesLegiveis(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}
