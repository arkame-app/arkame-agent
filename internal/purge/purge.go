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
//   - item de desbaste ("thinning") que aponta para a versão ATUAL da chave é
//     recusado. Desbaste só existe para versões antigas; a atual é o arquivo
//     como ele está hoje. Em 10/2026 o plano do painel chegou a listar a
//     versão atual de arquivos que não mudavam — apagá-la deixaria a chave sem
//     a cópia do estado presente. O agent confere com HeadObject (sem
//     VersionId) antes de apagar. "hard_delete" (o arquivo saiu da origem e
//     passou do prazo) e "deleted_file" (o arquivo saiu da origem e o cliente
//     desligou "manter a última versão de arquivos apagados") apagam a atual
//     de propósito e não passam por aqui — mas continuam exigindo o VersionId
//     exato e o prefixo.
//   - item com motivo desconhecido é recusado. Um painel mais novo pode
//     inventar um motivo cuja regra este agent não conhece; apagar sem saber
//     a regra é apostar com o dado do cliente.
//
// Um item recusado volta como falha, com o motivo. O painel registra e
// ninguém descobre meses depois que a limpeza estava apagando o que não devia.
package purge

import (
	"context"
	"errors"
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

	aceitas, recusadas := protegerVersaoAtual(ctx, o, aceitas)
	failed = append(failed, recusadas...)

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

// Motivos que o painel manda. Só o desbaste é restrito a versões antigas; os
// outros dois apagam a versão atual de propósito.
const (
	motivoDesbaste       = "thinning"
	motivoHardDelete     = "hard_delete"
	motivoArquivoApagado = "deleted_file"
)

// protegerVersaoAtual recusa os itens de desbaste que apontam para a versão
// atual da chave. Um HeadObject por chave (sem VersionId) diz qual é a atual.
//
// Na dúvida, recusa: se o HeadObject falhar por outro motivo que não "não
// existe", o item volta como falha e a próxima rodada tenta de novo. Chave
// sem versão atual (404: só versões antigas ou delete marker no topo) não tem
// o que proteger.
func protegerVersaoAtual(ctx context.Context, o Options, itens []Version) (aceitas []Version, recusadas []Failure) {
	type atual struct {
		versionID string
		err       error
	}
	consultadas := map[string]atual{}
	aceitas = itens[:0:0]
	for _, v := range itens {
		if v.Reason != motivoDesbaste {
			aceitas = append(aceitas, v)
			continue
		}
		a, ok := consultadas[v.Key]
		if !ok {
			a.versionID, a.err = versaoAtual(ctx, o, v.Key)
			consultadas[v.Key] = a
		}
		switch {
		case a.err != nil:
			recusadas = append(recusadas, Failure{Key: v.Key, VersionID: v.VersionID,
				Error: "desbaste recusado: não consegui conferir a versão atual da chave: " + resumir(a.err)})
		case a.versionID != "" && a.versionID == v.VersionID:
			recusadas = append(recusadas, Failure{Key: v.Key, VersionID: v.VersionID,
				Error: "desbaste recusado: a versão pedida é a versão atual da chave (desbaste só apaga versões antigas)"})
		default:
			aceitas = append(aceitas, v)
		}
	}
	return aceitas, recusadas
}

// versaoAtual devolve o VersionId da versão atual da chave, ou "" se a chave
// não tem versão atual (404).
func versaoAtual(ctx context.Context, o Options, key string) (string, error) {
	out, err := o.S3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(o.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return "", nil
		}
		var re interface{ HTTPStatusCode() int }
		// Delete marker no topo: o HeadObject sem versão responde 404 (às vezes
		// 405); em ambos a chave não tem versão atual a proteger.
		if errors.As(err, &re) && (re.HTTPStatusCode() == 404 || re.HTTPStatusCode() == 405) {
			return "", nil
		}
		return "", err
	}
	if out.VersionId == nil {
		return "", nil
	}
	return *out.VersionId, nil
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
	switch v.Reason {
	case motivoDesbaste, motivoHardDelete, motivoArquivoApagado:
	default:
		return fmt.Errorf("motivo desconhecido (%q): este agent não sabe a regra e não apaga", v.Reason)
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
