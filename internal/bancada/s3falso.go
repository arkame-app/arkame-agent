// Package bancada reúne o que os testes de escala e a ferramenta
// cmd/arkame-bancada usam para exercitar o agente sem bucket nem painel de
// verdade: um S3 falso que conta as requisições por operação, um painel falso
// que implementa o contrato do índice em lotes, e um gerador de árvores
// sintéticas com mutações.
//
// Nada daqui entra no programa do agente.
package bancada

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// VersaoFalsa é uma versão de objeto no S3 falso.
type VersaoFalsa struct {
	ID      string
	SHA256  string // metadado x-amz-meta-sha256
	Tamanho int64
	Dados   []byte // só com GuardarDados
}

// S3Falso é um S3 versionado em memória, com o que o agente usa: HeadObject,
// PutObject, multipart, DeleteObject (por versão), ListObjectVersions e as
// consultas de configuração do bucket. Conta cada requisição por operação.
type S3Falso struct {
	// GuardarDados guarda o conteúdo enviado (para conferir e para o
	// GetObject). Desligado, só o tamanho e o sha256 calculado ficam.
	GuardarDados bool
	// SemMemoria não guarda objeto nenhum: todo HeadObject sem versão
	// responde 404 e todo envio devolve uma versão. Para medir a memória do
	// agente sem a do bucket falso no mesmo processo. O id da versão leva o
	// sha256 e o tamanho do que subiu, e o HeadObject COM versão responde por
	// ele — a versão "existe" sem o bucket guardar nada.
	SemMemoria bool
	antes      atomic.Pointer[func(op string, r *http.Request)]
	falhar     atomic.Pointer[func(op string, r *http.Request) bool]

	mu       sync.Mutex
	objetos  map[string][]VersaoFalsa // chave → versões, a atual por último
	partes   map[string]*multipartFalso
	seq      int64
	ordenado []string // chaves em ordem, para a listagem; nil = refazer

	contagem          map[string]*atomic.Int64
	contMu            sync.Mutex
	bytesRecebidos    atomic.Int64
	multipartsAbertos atomic.Int64
	maxMultiparts     atomic.Int64
	emVoo             atomic.Int64
	maxEmVoo          atomic.Int64
}

type multipartFalso struct {
	chave  string
	sha    string
	partes map[int][]byte
}

// NovoS3Falso cria o bucket falso.
func NovoS3Falso() *S3Falso {
	return &S3Falso{
		objetos:  map[string][]VersaoFalsa{},
		partes:   map[string]*multipartFalso{},
		contagem: map[string]*atomic.Int64{},
	}
}

// ClienteS3 devolve um cliente do SDK apontado para endpoint (o servidor do
// S3Falso), configurado como o agente configura um S3 compatível.
func ClienteS3(endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
}

// Antes define o que roda no começo de cada requisição, com a operação já
// classificada (nil desliga).
func (f *S3Falso) Antes(fn func(op string, r *http.Request)) {
	if fn == nil {
		f.antes.Store(nil)
		return
	}
	f.antes.Store(&fn)
}

// Falhar define quem decide responder 500 a uma requisição (nil desliga).
func (f *S3Falso) Falhar(fn func(op string, r *http.Request) bool) {
	if fn == nil {
		f.falhar.Store(nil)
		return
	}
	f.falhar.Store(&fn)
}

func (f *S3Falso) contar(op string) {
	f.contMu.Lock()
	c := f.contagem[op]
	if c == nil {
		c = &atomic.Int64{}
		f.contagem[op] = c
	}
	f.contMu.Unlock()
	c.Add(1)
}

// Requisicoes devolve quantas requisições chegaram por operação.
func (f *S3Falso) Requisicoes() map[string]int64 {
	f.contMu.Lock()
	defer f.contMu.Unlock()
	out := make(map[string]int64, len(f.contagem))
	for k, v := range f.contagem {
		out[k] = v.Load()
	}
	return out
}

// N é quantas requisições chegaram da operação.
func (f *S3Falso) N(op string) int64 { return f.Requisicoes()[op] }

// Zerar zera os contadores (não os objetos).
func (f *S3Falso) Zerar() {
	f.contMu.Lock()
	f.contagem = map[string]*atomic.Int64{}
	f.contMu.Unlock()
	f.bytesRecebidos.Store(0)
	f.maxMultiparts.Store(0)
	f.maxEmVoo.Store(0)
}

// BytesRecebidos é quanto corpo de PutObject e UploadPart chegou.
func (f *S3Falso) BytesRecebidos() int64 { return f.bytesRecebidos.Load() }

// MaxMultiparts é o pico de envios multipart abertos ao mesmo tempo.
func (f *S3Falso) MaxMultiparts() int64 { return f.maxMultiparts.Load() }

// MaxEmVoo é o pico de requisições simultâneas.
func (f *S3Falso) MaxEmVoo() int64 { return f.maxEmVoo.Load() }

// Atual devolve a versão atual da chave.
func (f *S3Falso) Atual(chave string) (VersaoFalsa, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vs := f.objetos[chave]
	if len(vs) == 0 {
		return VersaoFalsa{}, false
	}
	return vs[len(vs)-1], true
}

// Versoes devolve as versões da chave, a atual por último.
func (f *S3Falso) Versoes(chave string) []VersaoFalsa {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]VersaoFalsa(nil), f.objetos[chave]...)
}

// Chaves devolve as chaves com alguma versão, em ordem.
func (f *S3Falso) Chaves() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.chavesOrdenadas()...)
}

// TotalDeVersoes soma as versões de todas as chaves.
func (f *S3Falso) TotalDeVersoes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, vs := range f.objetos {
		n += len(vs)
	}
	return n
}

// ApagarVersao tira uma versão direto do bucket, por fora do agente — o
// "alguém apagou o objeto" dos testes. Devolve se achou.
func (f *S3Falso) ApagarVersao(chave, versao string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apagar(chave, versao)
}

func (f *S3Falso) apagar(chave, versao string) bool {
	vs := f.objetos[chave]
	for i, v := range vs {
		if v.ID == versao {
			vs = append(vs[:i:i], vs[i+1:]...)
			if len(vs) == 0 {
				delete(f.objetos, chave)
				f.ordenado = nil
			} else {
				f.objetos[chave] = vs
			}
			return true
		}
	}
	return false
}

func (f *S3Falso) chavesOrdenadas() []string {
	if f.ordenado == nil {
		f.ordenado = make([]string, 0, len(f.objetos))
		for k := range f.objetos {
			f.ordenado = append(f.ordenado, k)
		}
		sort.Strings(f.ordenado)
	}
	return f.ordenado
}

func (f *S3Falso) gravar(chave string, v VersaoFalsa) string {
	f.seq++
	v.ID = fmt.Sprintf("v%08d", f.seq)
	if f.SemMemoria {
		return fmt.Sprintf("%s.%s.%d", v.ID, v.SHA256, v.Tamanho)
	}
	if _, tem := f.objetos[chave]; !tem {
		f.ordenado = nil
	}
	f.objetos[chave] = append(f.objetos[chave], v)
	return v.ID
}

func classificar(r *http.Request, chave string) string {
	q := r.URL.Query()
	switch {
	case chave == "" && r.Method == http.MethodGet && q.Has("versioning"):
		return "GetBucketVersioning"
	case chave == "" && r.Method == http.MethodGet && q.Has("object-lock"):
		return "GetObjectLockConfiguration"
	case chave == "" && r.Method == http.MethodGet && q.Has("lifecycle"):
		return "GetBucketLifecycleConfiguration"
	case chave == "" && r.Method == http.MethodGet && q.Has("versions"):
		return "ListObjectVersions"
	case chave == "" && r.Method == http.MethodPost && q.Has("delete"):
		return "DeleteObjects"
	case chave == "" && r.Method == http.MethodHead:
		return "HeadBucket"
	case r.Method == http.MethodHead && q.Get("versionId") != "":
		// A conferência de uma versão registrada (a amostra do estado local),
		// contada à parte do HeadObject por arquivo do backup.
		return "HeadObjectVersion"
	case r.Method == http.MethodHead:
		return "HeadObject"
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		return "UploadPart"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodPost && q.Has("uploads"):
		return "CreateMultipartUpload"
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		return "CompleteMultipartUpload"
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		return "AbortMultipartUpload"
	case r.Method == http.MethodDelete:
		return "DeleteObject"
	}
	return "Outra"
}

func subir(atual, pico *atomic.Int64) {
	n := atual.Add(1)
	for {
		p := pico.Load()
		if n <= p || pico.CompareAndSwap(p, n) {
			return
		}
	}
}

func erroS3(w http.ResponseWriter, status int, codigo string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, codigo, codigo)
}

func (f *S3Falso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	partes := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	chave := ""
	if len(partes) == 2 {
		chave = partes[1]
	}
	op := classificar(r, chave)
	f.contar(op)
	subir(&f.emVoo, &f.maxEmVoo)
	defer f.emVoo.Add(-1)
	if fn := f.antes.Load(); fn != nil {
		(*fn)(op, r)
	}
	if fn := f.falhar.Load(); fn != nil && (*fn)(op, r) {
		_, _ = io.Copy(io.Discard, r.Body)
		erroS3(w, http.StatusInternalServerError, "InternalError")
		return
	}
	q := r.URL.Query()

	// O corpo é lido fora da trava: envios simultâneos não se enfileiram.
	var corpo []byte
	var tamanho int64
	var sha string
	if op == "PutObject" || op == "UploadPart" {
		h := sha256.New()
		if f.GuardarDados || op == "UploadPart" {
			corpo, _ = io.ReadAll(io.TeeReader(r.Body, h))
			tamanho = int64(len(corpo))
		} else {
			tamanho, _ = io.Copy(h, r.Body)
		}
		sha = hex.EncodeToString(h.Sum(nil))
		f.bytesRecebidos.Add(tamanho)
	} else if op == "CompleteMultipartUpload" || op == "DeleteObjects" {
		corpo, _ = io.ReadAll(r.Body)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case "GetBucketVersioning":
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	case "GetObjectLockConfiguration":
		erroS3(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
	case "GetBucketLifecycleConfiguration":
		erroS3(w, http.StatusNotFound, "NoSuchLifecycleConfiguration")
	case "HeadBucket":
		w.WriteHeader(http.StatusOK)
	case "ListObjectVersions":
		f.listar(w, q)
	case "HeadObject", "HeadObjectVersion", "GetObject":
		vs := f.objetos[chave]
		var v *VersaoFalsa
		if id := q.Get("versionId"); id != "" {
			for i := range vs {
				if vs[i].ID == id {
					v = &vs[i]
				}
			}
		} else if len(vs) > 0 {
			v = &vs[len(vs)-1]
		}
		if v == nil && f.SemMemoria && op == "HeadObjectVersion" {
			if p := strings.Split(q.Get("versionId"), "."); len(p) == 3 {
				t, _ := strconv.ParseInt(p[2], 10, 64)
				v = &VersaoFalsa{ID: q.Get("versionId"), SHA256: p[1], Tamanho: t}
			}
		}
		if v == nil {
			if op == "GetObject" {
				erroS3(w, http.StatusNotFound, "NoSuchKey")
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		w.Header().Set("x-amz-meta-sha256", v.SHA256)
		w.Header().Set("x-amz-version-id", v.ID)
		w.Header().Set("Content-Length", strconv.FormatInt(v.Tamanho, 10))
		w.WriteHeader(http.StatusOK)
		if op == "GetObject" {
			_, _ = w.Write(v.Dados)
		}
	case "PutObject":
		_ = sha
		id := f.gravar(chave, VersaoFalsa{SHA256: r.Header.Get("x-amz-meta-sha256"), Tamanho: tamanho, Dados: corpo})
		w.Header().Set("x-amz-version-id", id)
		w.WriteHeader(http.StatusOK)
	case "CreateMultipartUpload":
		f.seq++
		id := fmt.Sprintf("up%d", f.seq)
		f.partes[id] = &multipartFalso{chave: chave, sha: r.Header.Get("x-amz-meta-sha256"), partes: map[int][]byte{}}
		subir(&f.multipartsAbertos, &f.maxMultiparts)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, xmlTexto(chave), id)
	case "UploadPart":
		mp := f.partes[q.Get("uploadId")]
		if mp == nil {
			erroS3(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if f.GuardarDados {
			mp.partes[n] = corpo
		} else {
			mp.partes[n] = []byte(strconv.FormatInt(tamanho, 10))
		}
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
		w.WriteHeader(http.StatusOK)
	case "CompleteMultipartUpload":
		mp := f.partes[q.Get("uploadId")]
		if mp == nil {
			erroS3(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		delete(f.partes, q.Get("uploadId"))
		f.multipartsAbertos.Add(-1)
		var nums []int
		for n := range mp.partes {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		v := VersaoFalsa{SHA256: mp.sha}
		for _, n := range nums {
			if f.GuardarDados {
				v.Dados = append(v.Dados, mp.partes[n]...)
				v.Tamanho += int64(len(mp.partes[n]))
			} else {
				t, _ := strconv.ParseInt(string(mp.partes[n]), 10, 64)
				v.Tamanho += t
			}
		}
		id := f.gravar(chave, v)
		w.Header().Set("x-amz-version-id", id)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`, xmlTexto(chave))
	case "AbortMultipartUpload":
		if _, tem := f.partes[q.Get("uploadId")]; tem {
			delete(f.partes, q.Get("uploadId"))
			f.multipartsAbertos.Add(-1)
		}
		w.WriteHeader(http.StatusNoContent)
	case "DeleteObject":
		if id := q.Get("versionId"); id != "" {
			f.apagar(chave, id)
		}
		w.WriteHeader(http.StatusNoContent)
	case "DeleteObjects":
		var pedido struct {
			Objetos []struct {
				Key       string `xml:"Key"`
				VersionID string `xml:"VersionId"`
			} `xml:"Object"`
		}
		_ = xml.Unmarshal(corpo, &pedido)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult>`)
		for _, o := range pedido.Objetos {
			f.apagar(o.Key, o.VersionID)
			fmt.Fprintf(&b, `<Deleted><Key>%s</Key><VersionId>%s</VersionId></Deleted>`, xmlTexto(o.Key), xmlTexto(o.VersionID))
		}
		b.WriteString(`</DeleteResult>`)
		fmt.Fprint(w, b.String())
	default:
		erroS3(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func xmlTexto(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// listar responde o ListObjectVersions: mil versões por página, em ordem de
// chave, a versão atual primeiro.
func (f *S3Falso) listar(w http.ResponseWriter, q url.Values) {
	const pagina = 1000
	chaves := f.chavesOrdenadas()
	marcaChave, marcaVersao := q.Get("key-marker"), q.Get("version-id-marker")
	prefixo := q.Get("prefix")
	i := sort.SearchStrings(chaves, marcaChave)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListVersionsResult><Name>b</Name>`)
	n := 0
	truncado := false
	var proxChave, proxVersao string
fora:
	for ; i < len(chaves); i++ {
		k := chaves[i]
		if prefixo != "" && !strings.HasPrefix(k, prefixo) {
			continue
		}
		vs := f.objetos[k]
		pular := k == marcaChave && marcaVersao != ""
		if k == marcaChave && marcaVersao == "" && marcaChave != "" {
			continue // a chave inteira já saiu
		}
		for j := len(vs) - 1; j >= 0; j-- {
			if pular {
				if vs[j].ID == marcaVersao {
					pular = false
				}
				continue
			}
			if n == pagina {
				truncado = true
				break fora
			}
			fmt.Fprintf(&b, `<Version><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><Size>%d</Size></Version>`,
				xmlTexto(k), vs[j].ID, j == len(vs)-1, vs[j].Tamanho)
			n++
			proxChave, proxVersao = k, vs[j].ID
		}
	}
	fmt.Fprintf(&b, `<IsTruncated>%t</IsTruncated>`, truncado)
	if truncado {
		fmt.Fprintf(&b, `<NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>%s</NextVersionIdMarker>`, xmlTexto(proxChave), proxVersao)
	}
	b.WriteString(`</ListVersionsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, b.String())
}
