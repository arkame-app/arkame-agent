package purge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	gosync "sync"
	"testing"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

// planoDeDoisLotes monta loteMáximo+n versões de hard_delete (sem HeadObject)
// no s3Falso: o Run as manda em dois DeleteObjects.
func planoDeDoisLotes(f *s3Falso, n int) []Version {
	var vs []Version
	for i := 0; i < loteMáximo+n; i++ {
		key := fmt.Sprintf("arkame/data/a/%05d", i)
		f.versoes[key] = []string{"v1"}
		vs = append(vs, Version{Key: key, VersionID: "v1", Reason: "hard_delete"})
	}
	return vs
}

// O serviço para durante o DeleteObjects do segundo lote: o primeiro já saiu
// e volta em deleted; o segundo, cortado, não vai para failed — não se sabe
// se o provedor o apagou, e a parada do serviço não é erro do bucket.
func TestRunCanceladoNoLoteEmVooNaoMarcaFalha(t *testing.T) {
	f, c := novoS3Falso(t)
	versoes := planoDeDoisLotes(f, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.antesDoDelete = func(n int, r *http.Request) bool {
		if n < 2 {
			return true
		}
		// O servidor só percebe a conexão fechada depois de ler o corpo.
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done() // o cliente desiste com o ctx cancelado
		return false
	}

	deleted, failed := Run(ctx, Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"}, versoes)

	if len(deleted) != loteMáximo {
		t.Fatalf("esperava o primeiro lote inteiro em deleted (%d), veio %d", loteMáximo, len(deleted))
	}
	if len(failed) != 0 {
		t.Fatalf("o lote cortado pela parada não pode voltar como falha; veio %d falhas, a primeira: %+v",
			len(failed), failed[0])
	}
}

// O serviço para entre dois lotes: o primeiro DeleteObjects termina, o ctx é
// cancelado logo depois (num middleware do SDK, para o cancelamento não
// cortar a própria resposta) e o segundo lote nem é enviado.
func TestRunCanceladoEntreLotesDevolveOPrimeiro(t *testing.T) {
	f, c := novoS3Falso(t)
	versoes := planoDeDoisLotes(f, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c = s3.New(c.Options(), cancelarDepoisDoPrimeiroDelete(cancel))
	f.antesDoDelete = func(n int, r *http.Request) bool {
		if n > 1 {
			t.Errorf("o segundo lote foi enviado com o serviço parando")
		}
		return true
	}

	deleted, failed := Run(ctx, Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"}, versoes)

	if len(deleted) != loteMáximo || len(failed) != 0 {
		t.Fatalf("esperava %d apagadas e nenhuma falha, veio %d e %d", loteMáximo, len(deleted), len(failed))
	}
}

// cancelarDepoisDoPrimeiroDelete chama cancel quando o primeiro DeleteObjects
// do cliente termina, com a resposta já lida.
func cancelarDepoisDoPrimeiroDelete(cancel context.CancelFunc) func(*s3.Options) {
	var uma gosync.Once
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(st *middleware.Stack) error {
			return st.Initialize.Add(middleware.InitializeMiddlewareFunc("cancelaDepoisDoPrimeiroLote",
				func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
					out, md, err := next.HandleInitialize(ctx, in)
					if awsmiddleware.GetOperationName(ctx) == "DeleteObjects" {
						uma.Do(cancel)
					}
					return out, md, err
				}), middleware.After)
		})
	}
}
