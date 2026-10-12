package sync

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	gosync "sync"

	"github.com/arkame-app/agent/internal/estado"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// AmostraPadrao é quantas entradas do estado local são conferidas no bucket
// no começo de cada execução, antes de qualquer atalho.
const AmostraPadrao = 32

// O atalho afirma, sem consultar o bucket, que a versão registrada no estado
// continua lá. Isso deixa de ser verdade quando alguém mexe no bucket por
// fora: versões apagadas, bucket recriado com o mesmo nome, armazenamento
// reapontado para outro endereço, regra de ciclo de vida. Sem conferência, o
// backup seguinte fechava "complete" sem tocar o bucket, com o painel
// afirmando arquivos que não existem.
//
// Por isso toda execução que vai usar o atalho começa conferindo uma amostra
// sorteada das entradas que valem: um HeadObject com o VersionId registrado,
// que tem de devolver o mesmo sha256 e o mesmo tamanho. Uma só que falhe —
// versão que sumiu, conteúdo diferente, ou o bucket que não respondeu — e a
// execução INTEIRA deixa de confiar no estado: lê e confere todo arquivo
// (RehashTotal), como a v0.4.31. O que sumiu sobe de novo; o que está lá não
// é reenviado.
//
// A amostra é fixa (AmostraPadrao), não cresce com o plano: bucket esvaziado,
// recriado ou trocado derruba todas as entradas e é pego com certeza; perda
// parcial é pega com a probabilidade da fração perdida (10% → 97% numa
// execução; 1% → 28% por execução, 96% em dez), e a reconferência rotativa
// cobre o resto no ciclo dela.
//
// Não há objeto-sentinela: ele pegaria os mesmos casos em que TODAS as
// entradas caem (que a amostra já pega), não pegaria perda parcial, exigiria
// gravar fora do prefixo dos dados (permissão, regra de ciclo de vida que o
// apagasse → alarme falso para sempre) e não existe em bucket em que o agente
// só tem leitura das versões.

// amostraDoEstado sorteia até n entradas que serviriam de atalho.
func amostraDoEstado(ex *estado.Execucao, n int, sortear func(int) int) ([]estado.Gravacao, error) {
	var amostra []estado.Gravacao
	candidatas := 0
	err := ex.Varrer(func(rel string, ent estado.Entrada, _ bool) (estado.Acao, estado.Entrada) {
		if ent.Removida || ent.Versao == "" || !ex.Vale(ent) {
			return estado.Manter, ent
		}
		// Reservatório: cada candidata fica com a mesma chance, numa passada.
		candidatas++
		if len(amostra) < n {
			amostra = append(amostra, estado.Gravacao{Chave: rel, Ent: ent})
		} else if j := sortear(candidatas); j < n {
			amostra[j] = estado.Gravacao{Chave: rel, Ent: ent}
		}
		return estado.Manter, ent
	})
	return amostra, err
}

// conferirAmostra confere a amostra no bucket. Devolve quantas entradas
// foram consultadas e, se alguma não conferiu, o motivo ("" = todas certas,
// ou nada a conferir). Com o contexto cancelado não conclui nada.
func conferirAmostra(ctx context.Context, o EngineOptions) (consultadas int, motivo string) {
	n := o.Amostra
	if n == 0 {
		n = AmostraPadrao
	}
	if n < 0 || o.Estado == nil {
		return 0, ""
	}
	sortear := o.Sortear
	if sortear == nil {
		sortear = rand.IntN
	}
	amostra, err := amostraDoEstado(o.Estado, n, sortear)
	if err != nil {
		return 0, "o estado local não pôde ser lido: " + err.Error()
	}
	if len(amostra) == 0 {
		return 0, ""
	}
	prefixo := PrefixoDasChaves(o.PrefixRoot)
	trabalhadores := min(len(amostra), 8)
	itens := make(chan estado.Gravacao)
	var mu gosync.Mutex
	var wg gosync.WaitGroup
	for i := 0; i < trabalhadores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range itens {
				// Uma falha basta: as que ainda não saíram não saem.
				mu.Lock()
				parar := motivo != ""
				mu.Unlock()
				if parar {
					continue
				}
				falha := conferirVersao(ctx, o.S3, o.Bucket, prefixo+it.Chave, it.Ent)
				mu.Lock()
				consultadas++
				if falha != "" && motivo == "" {
					motivo = falha
				}
				mu.Unlock()
			}
		}()
	}
	for _, it := range amostra {
		if ctx.Err() != nil {
			break
		}
		itens <- it
	}
	close(itens)
	wg.Wait()
	if ctx.Err() != nil {
		return consultadas, ""
	}
	return consultadas, motivo
}

// conferirVersao diz por que a versão registrada de uma chave não confere
// com o bucket ("" = confere).
func conferirVersao(ctx context.Context, s3c *s3.Client, bucket, chave string, ent estado.Entrada) string {
	versao := ent.Versao
	head, err := s3c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &chave, VersionId: &versao})
	if err != nil {
		return fmt.Sprintf("%s (versão %s): %v", chave, versao, err)
	}
	if sha := head.Metadata["sha256"]; sha != hex.EncodeToString(ent.SHA256[:]) {
		return fmt.Sprintf("%s (versão %s): o objeto tem outro sha256", chave, versao)
	}
	if head.ContentLength == nil || *head.ContentLength != ent.Tamanho {
		return fmt.Sprintf("%s (versão %s): o objeto tem outro tamanho", chave, versao)
	}
	return ""
}

// avisarAmostra registra a amostra que não conferiu.
func avisarAmostra(consultadas int, motivo string) {
	slog.Warn("o bucket não confere com o estado local; este backup lê e confere todos os arquivos",
		"entradas_consultadas", consultadas, "primeira_falha", motivo)
}
