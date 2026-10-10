// Package sync implementa o engine de backup (walker + diff + upload).
package sync

import (
	"context"
	"errors"
	"io"

	"golang.org/x/time/rate"
)

// ThrottledReader envolve um io.Reader com rate limit de bytes/sec.
// Sem limitador, não aplica throttle.
type ThrottledReader struct {
	r       io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

// novoLimitador é o limite de banda de uma execução, em bytes por segundo,
// com um segundo de rajada. nil quando não há limite.
func novoLimitador(maxMbps int) *rate.Limiter {
	if maxMbps <= 0 {
		return nil
	}
	bytesPerSec := rate.Limit(maxMbps * 1024 * 1024 / 8)
	return rate.NewLimiter(bytesPerSec, int(bytesPerSec))
}

// NewThrottledReader limita a leitura de r a maxMbps, com um limitador só
// deste leitor. O backup usa leitorLimitado, com o limitador da execução: com
// vários envios ao mesmo tempo, um limitador por leitor multiplicaria o limite
// pelo número de envios.
func NewThrottledReader(r io.Reader, maxMbps int) *ThrottledReader {
	return leitorLimitado(context.Background(), r, novoLimitador(maxMbps))
}

// leitorLimitado limita a leitura de r pelo limitador dado (o da execução,
// compartilhado por todos os envios). A espera acaba com o ctx.
func leitorLimitado(ctx context.Context, r io.Reader, lim *rate.Limiter) *ThrottledReader {
	return &ThrottledReader{r: r, limiter: lim, ctx: ctx}
}

func (tr *ThrottledReader) Read(p []byte) (int, error) {
	if tr.limiter == nil {
		return tr.r.Read(p)
	}
	// Nunca mais que a rajada de uma vez: o WaitN recusa pedido maior que
	// ela (e o erro, ignorado, deixava a leitura passar sem limite).
	if b := tr.limiter.Burst(); len(p) > b {
		p = p[:b]
	}
	n, err := tr.r.Read(p)
	if n > 0 {
		if werr := tr.limiter.WaitN(tr.ctx, n); werr != nil && tr.ctx.Err() != nil {
			return n, tr.ctx.Err()
		}
	}
	return n, err
}

// Seek delega ao reader subjacente quando ele é seekável (ex.: *os.File).
// Expor io.ReadSeeker permite ao AWS SDK descobrir o tamanho do corpo,
// enviar Content-Length (S3-compat como OCI exige) e reusar o corpo em retries.
func (tr *ThrottledReader) Seek(offset int64, whence int) (int64, error) {
	if s, ok := tr.r.(io.Seeker); ok {
		return s.Seek(offset, whence)
	}
	return 0, errors.New("reader subjacente não suporta seek")
}
