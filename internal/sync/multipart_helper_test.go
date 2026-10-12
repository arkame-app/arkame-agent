package sync

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// uploadMultipart é o uploadMultipartCom com um limitador de banda próprio,
// como era antes de o limite passar a ser um por execução. Só os testes do
// multipart o usam.
func uploadMultipart(ctx context.Context, s3c *s3.Client, bucket, key string, f *os.File, meta map[string]string, maxMbps int, hashEsperado string, tamanho int64) (string, error) {
	return uploadMultipartCom(ctx, s3c, bucket, key, f, meta, novoLimitador(maxMbps), hashEsperado, tamanho)
}
