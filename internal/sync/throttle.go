// Package sync implementa o engine de backup (walker + diff + upload).
package sync

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// ThrottledReader envolve um io.Reader com rate limit de bytes/sec.
// Se maxMbps <= 0, não aplica throttle.
type ThrottledReader struct {
	r       io.Reader
	limiter *rate.Limiter
}

func NewThrottledReader(r io.Reader, maxMbps int) *ThrottledReader {
	if maxMbps <= 0 {
		return &ThrottledReader{r: r}
	}
	// maxMbps → bytes/sec
	bytesPerSec := rate.Limit(maxMbps * 1024 * 1024 / 8)
	// burst = 1 segundo de capacidade
	burst := int(bytesPerSec)
	return &ThrottledReader{
		r:       r,
		limiter: rate.NewLimiter(bytesPerSec, burst),
	}
}

func (tr *ThrottledReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 && tr.limiter != nil {
		// Pede reserva; se exceder o burst, espera
		_ = tr.limiter.WaitN(context.Background(), n)
	}
	return n, err
}
