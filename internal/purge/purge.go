// Package purge executa a retenção: apaga do bucket as versões que o painel
// autorizou.
//
// O painel decide o quê e o agent executa, porque as credenciais do storage
// são do cliente e ficam só aqui. Isso significa que este código é o último
// ponto antes de um dado do cliente deixar de existir — e por isso ele
// desconfia do plano que recebe em vez de obedecê-lo cegamente:
//
//   - item sem VersionId é recusado. Um DeleteObject sem versão não apaga uma
//     versão antiga: ele cria um delete marker e some com o arquivo atual.
//     Um plano malformado viraria perda de dados silenciosa.
//   - item fora do prefixo do storage é recusado. O bucket é do cliente e
//     pode ter muita coisa que não é nossa.
//
// Um item recusado volta como falha, com o motivo. O painel registra e
// ninguém descobre meses depois que a limpeza estava apagando o que não devia.
package purge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Version é uma versão de objeto que o painel autorizou apagar.
type Version struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
	Reason    string `json:"reason"`
}

// Failure descreve uma versão que não saiu, e por quê.
type Failure struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Error     string `json:"error"`
}

// Options reúne o que uma rodada precisa saber.
type Options struct {
	S3         *s3.Client
	Bucket     string
	PrefixRoot string
}

// loteMáximo é o teto do DeleteObjects no protocolo S3.
const loteMáximo = 1000

// Run apaga as versões do plano e devolve o que saiu e o que falhou.
//
// Erros de rede não interrompem a rodada: o que falhou volta como falha e a
// próxima rodada recalcula. Uma limpeza que aborta na primeira falha nunca
// termina em bucket grande.
func Run(ctx context.Context, o Options, versions []Version) (deleted []Version, failed []Failure) {
	aceitas := make([]Version, 0, len(versions))
	for _, v := range versions {
		if err := validar(o, v); err != nil {
			failed = append(failed, Failure{Key: v.Key, VersionID: v.VersionID, Error: err.Error()})
			continue
		}
		aceitas = append(aceitas, v)
	}

	if len(failed) > 0 {
		slog.Warn("itens do plano recusados pelo agent", "count", len(failed), "primeiro", failed[0].Error)
	}

	porChave := make(map[string]Version, len(aceitas))
	for _, v := range aceitas {
		porChave[v.Key+"\x00"+v.VersionID] = v
	}

	for início := 0; início < len(aceitas); início += loteMáximo {
		if ctx.Err() != nil {
			return deleted, failed
		}
		fim := início + loteMáximo
		if fim > len(aceitas) {
			fim = len(aceitas)
		}
		lote := aceitas[início:fim]

		objetos := make([]types.ObjectIdentifier, 0, len(lote))
		for _, v := range lote {
			objetos = append(objetos, types.ObjectIdentifier{
				Key:       aws.String(v.Key),
				VersionId: aws.String(v.VersionID),
			})
		}

		out, err := o.S3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(o.Bucket),
			Delete: &types.Delete{Objects: objetos, Quiet: aws.Bool(false)},
		})
		if err != nil {
			// A chamada inteira falhou (rede, credencial, permissão). Marca o
			// lote como falho e segue: o resto do plano ainda pode sair, e o
			// painel verá o motivo repetido em todas as linhas.
			for _, v := range lote {
				failed = append(failed, Failure{Key: v.Key, VersionID: v.VersionID, Error: resumir(err)})
			}
			continue
		}

		for _, d := range out.Deleted {
			if d.Key == nil || d.VersionId == nil {
				continue
			}
			if v, ok := porChave[*d.Key+"\x00"+*d.VersionId]; ok {
				deleted = append(deleted, v)
			}
		}
		for _, e := range out.Errors {
			f := Failure{Error: "erro sem detalhe"}
			if e.Key != nil {
				f.Key = *e.Key
			}
			if e.VersionId != nil {
				f.VersionID = *e.VersionId
			}
			if e.Message != nil {
				f.Error = *e.Message
			} else if e.Code != nil {
				f.Error = *e.Code
			}
			failed = append(failed, f)
		}
	}

	return deleted, failed
}

func validar(o Options, v Version) error {
	if v.VersionID == "" {
		return fmt.Errorf("item sem version_id: apagar sem versão criaria delete marker")
	}
	if v.Key == "" {
		return fmt.Errorf("item sem key")
	}
	if o.PrefixRoot != "" && !strings.HasPrefix(v.Key, o.PrefixRoot) {
		return fmt.Errorf("key fora do prefixo do storage (%s)", o.PrefixRoot)
	}
	return nil
}

// resumir corta a mensagem para caber no registro sem virar um romance.
func resumir(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		return msg[:300]
	}
	return msg
}
