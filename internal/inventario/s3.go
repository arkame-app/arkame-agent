package inventario

import (
	"context"
	"net/url"
	"sort"

	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/internal/verificacao"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// BucketS3 é o Bucket de verdade: o bucket do cliente, pelo SDK.
type BucketS3 struct {
	Cliente *s3.Client
	Nome    string
}

// Listar pede uma página do ListObjectVersions e junta versões e marcadores de
// exclusão — que o S3 devolve em duas listas — numa só, em ordem de chave.
//
// Pede as chaves codificadas (encoding-type=url): uma chave com caractere que
// o XML não carrega quebraria a resposta inteira. Só decodifica se o provedor
// disser que codificou; o que ignora o pedido devolve as chaves como são.
func (b BucketS3) Listar(ctx context.Context, prefixo, depoisDaChave, depoisDaVersao string) (Pagina, error) {
	in := &s3.ListObjectVersionsInput{Bucket: &b.Nome, Prefix: &prefixo, EncodingType: types.EncodingTypeUrl}
	if depoisDaChave != "" {
		in.KeyMarker = &depoisDaChave
		if depoisDaVersao != "" {
			in.VersionIdMarker = &depoisDaVersao
		}
	}
	out, err := b.Cliente.ListObjectVersions(ctx, in)
	if err != nil {
		return Pagina{}, err
	}
	codificada := out.EncodingType == types.EncodingTypeUrl
	chave := func(p *string) (string, error) {
		if p == nil {
			return "", nil
		}
		if !codificada {
			return *p, nil
		}
		return url.QueryUnescape(*p)
	}
	pg := Pagina{Entradas: make([]Entrada, 0, len(out.Versions)+len(out.DeleteMarkers))}
	for _, v := range out.Versions {
		k, err := chave(v.Key)
		if err != nil {
			return Pagina{}, err
		}
		e := Entrada{Key: k}
		if v.VersionId != nil {
			e.VersionID = *v.VersionId
		}
		if v.Size != nil {
			e.Size = *v.Size
		}
		if v.IsLatest != nil {
			e.Atual = *v.IsLatest
		}
		pg.Entradas = append(pg.Entradas, e)
	}
	for _, d := range out.DeleteMarkers {
		k, err := chave(d.Key)
		if err != nil {
			return Pagina{}, err
		}
		e := Entrada{Key: k, Marcador: true}
		if d.VersionId != nil {
			e.VersionID = *d.VersionId
		}
		if d.IsLatest != nil {
			e.Atual = *d.IsLatest
		}
		pg.Entradas = append(pg.Entradas, e)
	}
	// Cada lista vem em ordem de chave; juntas, não. Estável: a ordem das
	// versões de uma mesma chave fica como veio.
	if len(out.DeleteMarkers) > 0 {
		sort.SliceStable(pg.Entradas, func(i, j int) bool { return pg.Entradas[i].Key < pg.Entradas[j].Key })
	}
	if out.IsTruncated != nil && *out.IsTruncated {
		pg.Truncada = true
		k, err := chave(out.NextKeyMarker)
		if err != nil {
			return Pagina{}, err
		}
		pg.ProximaChave = k
		if out.NextVersionIdMarker != nil {
			pg.ProximaVersao = *out.NextVersionIdMarker
		}
	}
	return pg, nil
}

// Cabecalho faz o HeadObject de uma versão.
func (b BucketS3) Cabecalho(ctx context.Context, chave, versao string) (Cabecalho, error) {
	in := &s3.HeadObjectInput{Bucket: &b.Nome, Key: &chave}
	if versao != "" {
		in.VersionId = &versao
	}
	out, err := b.Cliente.HeadObject(ctx, in)
	if err != nil {
		return Cabecalho{}, err
	}
	h := Cabecalho{SHA256: out.Metadata["sha256"], LockMode: string(out.ObjectLockMode), RetainUntil: out.ObjectLockRetainUntilDate, LastModified: out.LastModified}
	if out.ContentLength != nil {
		h.Size = *out.ContentLength
	}
	return h, nil
}

// Versionamento devolve "Enabled", "Suspended" ou "Off".
func (b BucketS3) Versionamento(ctx context.Context) (string, error) {
	return storage.VersioningStatus(ctx, b.Cliente, b.Nome)
}

// Classificar usa a mesma régua do teste de restauração: só a resposta
// definitiva do bucket conta; o resto é do momento.
func (b BucketS3) Classificar(err error) ClasseDoErro {
	switch codigo, _ := verificacao.ClassificarErroDeS3(err); codigo {
	case verificacao.ErroNaoEncontrado:
		return ErroAusente
	case verificacao.ErroAcessoNegado:
		return ErroNegado
	case verificacao.ErroDeDownload:
		return ErroDefinitivo
	}
	return ErroDoMomento
}
