// Package restore implementa o executor de restore: dado um RestoreItem,
// baixa o objeto do bucket (GetObject com VersionId), valida sha256 e
// escreve atomicamente em destPath/destFilename com conflict resolution.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/caminho"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MultipartThresholdBytes — arquivos > 100 MB usam s3manager.Downloader (multipart paralelo)
const MultipartThresholdBytes = 100 * 1024 * 1024

// ErrWarmingRequested indica que o objeto está em cold storage e o executor
// já disparou um RestoreObject pra trazê-lo. O caller deve marcar warming_state
// e tentar de novo numa próxima rodada do loop.
type ErrWarmingRequested struct {
	// PrevistoEm é quando o objeto deve estar disponível, pelo contrato da AWS
	// para a combinação classe × tier. É teto, não média: melhor prometer menos.
	PrevistoEm   time.Time
	StorageClass string
	Tier         string
	Bucket       string
	Key          string
}

func (e *ErrWarmingRequested) Error() string {
	return fmt.Sprintf("warming requested: %s/%s (class=%s, tier=%s)", e.Bucket, e.Key, e.StorageClass, e.Tier)
}

// ErrWarmingInProgress indica que o objeto já está sendo warmed mas ainda não
// está disponível. Não dispara novo RestoreObject, só espera.
type ErrWarmingInProgress struct {
	StorageClass string
	Bucket       string
	Key          string
}

func (e *ErrWarmingInProgress) Error() string {
	return fmt.Sprintf("warming in progress: %s/%s (class=%s)", e.Bucket, e.Key, e.StorageClass)
}

// ErrPulado: a estratégia de conflito é "skip" e o destino já tem um arquivo
// com o nome (com outro conteúdo). Nada foi gravado. O daemon relata o item
// como "skipped": antes ele ia "complete", e o painel dizia restaurado um
// arquivo que não foi escrito.
var ErrPulado = errors.New("destino já existe; pulado pela estratégia skip")

// Options configura o executor.
type Options struct {
	S3       *s3.Client
	HostRoot string // raiz (em container Docker = "/host"; standalone = "/")
}

// Run baixa um item do bucket e escreve no destino. Se o destino já tem o
// item inteiro (mesmo tamanho e sha256 — um item refeito porque o resultado
// não chegou ao painel), não baixa nem grava de novo e devolve nil.
//
// Estratégia de conflito (item.ConflictStrategy):
//   - "suffix-version": se o arquivo existe, escreve como "<name>.v<versionId8>.<ext>"
//   - "overwrite": sobrescreve sem aviso
//   - "skip": se o arquivo existe, não escreve e devolve ErrPulado
//
// Em qualquer estratégia, a escrita é atômica: baixa pra arquivo temp no mesmo
// diretório e renomeia ao final. Se falhar no meio, o temp é removido.
//
// Valida SHA-256 do conteúdo baixado contra item.SourceSha256 antes de renomear.
func Run(ctx context.Context, opts Options, item api.RestoreItem) error {
	if opts.S3 == nil {
		return errors.New("s3 client é obrigatório")
	}
	if item.Bucket == "" || item.SourceKey == "" {
		return errors.New("item sem bucket ou source_key")
	}
	// No Windows, `C:\Restaurados` (ou o formato antigo `/restore`, no disco do
	// sistema); no resto, `/restore` dentro do HostRoot.
	destDir, err := caminho.Destino(opts.HostRoot, item.DestPath)
	if err != nil {
		return err
	}
	// dest_filename pode conter subdiretórios relativos (restore de pasta/snapshot
	// preserva a estrutura), mas nunca path absoluto, "..", ou backslash.
	subDir, baseName, err := splitDestFilename(item.DestFilename)
	if err != nil {
		return err
	}

	// A pasta é aberta sem seguir link de usuário (ErrDestinoLink): o agente
	// grava como root, e quem manda na pasta de destino poderia trocá-la por
	// um link para /root/.ssh. Os links do sistema (/home -> var/home no
	// Fedora Atomic) continuam valendo, e um link absoluto recomeça no
	// HostRoot, não dentro do container.
	noLugar := noLugarDeOrigem(caminho.Windows, caminho.NaChave(opts.HostRoot,
		filepath.Join(destDir, filepath.FromSlash(item.DestFilename))), item.SourceKey)
	dir, err := abrirPasta(opts.HostRoot, destDir, subDir, noLugar)
	if err != nil {
		return err
	}
	defer dir.Close()
	aposAbrirPasta()

	// Item refeito: a gravação deu certo, mas o PATCH final não chegou ao
	// painel, e o item voltou na fila. O resolveConflict via o arquivo que
	// esta mesma restauração acabou de gravar e, em suffix-version, gravava
	// outra cópia (app.conf.vXXXX.1). Já no destino, inteiro, é concluído.
	//
	// Daqui até o rename, tudo o que se lê ou grava no destino vai pelo
	// descritor da pasta aberta, nunca pelo caminho em texto: quem manda numa
	// pasta do caminho pode trocá-la por um link a qualquer momento.
	if feito, err := jaRestaurado(dir, baseName, item); err != nil {
		return err
	} else if feito {
		return nil
	}

	finalName, err := resolveConflict(dir, baseName, item.SourceVersionID, item.ConflictStrategy)
	if err != nil {
		return err
	}
	if finalName == "" {
		// skip: nada foi gravado, e o painel tem de saber disso.
		return ErrPulado
	}

	tmpFile, tmpNome, err := dir.criarTemp(padraoTemporario(finalName))
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			tmpFile.Close()
			dir.remover(tmpNome)
		}
	}()

	written, hashErr := downloadObject(ctx, opts.S3, item, tmpFile)
	if hashErr != nil {
		var iose *s3types.InvalidObjectState
		if errors.As(hashErr, &iose) {
			return handleColdStorage(ctx, opts.S3, item, iose)
		}
		return hashErr
	}

	if item.SourceSize > 0 && written.bytes != item.SourceSize {
		return fmt.Errorf("size mismatch: esperado=%d baixado=%d", item.SourceSize, written.bytes)
	}
	if item.SourceSha256 != "" && !strings.EqualFold(written.sha256, item.SourceSha256) {
		return fmt.Errorf("sha256 mismatch: esperado=%s baixado=%s", item.SourceSha256, written.sha256)
	}
	aposBaixar()

	// Por cima de um arquivo existente, ou ao lado dele (suffix-version), herda
	// o modo e o dono dele; arquivo novo fica 0600, do dono da pasta. Pelo
	// descritor, não pelo caminho.
	if err := ajustarPermissoes(tmpFile, dir, baseName, finalName); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("fechando o temporário: %w", err)
	}
	// A data do arquivo no backup, quando o painel a manda: a data de agora
	// faz todo arquivo restaurado parecer recém-alterado (backup incremental,
	// make, rsync e quem procura "o que mudou" passam a errar).
	mtime := time.Now()
	if item.SourceModifiedAt != nil && !item.SourceModifiedAt.IsZero() {
		mtime = *item.SourceModifiedAt
	}
	_ = dir.aplicarData(tmpNome, time.Now(), mtime)

	if err := dir.renomear(tmpNome, finalName); err != nil {
		dir.remover(tmpNome)
		cleanup = false
		return fmt.Errorf("rename tmp → final: %w", err)
	}
	cleanup = false
	return nil
}

// aposAbrirPasta e aposBaixar são os pontos do Run em que o teste troca uma
// pasta do caminho por um link, como faria quem manda nela durante a
// restauração.
var (
	aposAbrirPasta = func() {}
	aposBaixar     = func() {}
)

// modoDeArquivoNovo é o modo de um arquivo restaurado sem um existente de
// mesmo nome. O modo original não está no índice, e o arquivo pode ser um
// segredo (/etc/shadow, chave privada, .env): era 0644, e restaurar /etc para
// /restore deixava tudo isso legível por qualquer usuário. Quem precisa de
// outro modo o dá depois; o contrário não se desfaz.
const modoDeArquivoNovo os.FileMode = 0o600

// trocarDono é o copiarDono da plataforma; variável para o teste simular,
// sem root, o chown que limpa setuid/setgid.
var trocarDono = copiarDono

// ajustarPermissoes dá ao temporário o modo (e, como root fora do Windows, o
// dono) do arquivo existente com o nome do item (baseName): o que ele vai
// substituir (overwrite) ou ao lado do qual vai ficar (suffix-version, com
// finalName diferente). Sem arquivo regular com esse nome, modoDeArquivoNovo,
// com o dono e o grupo da pasta (fstat do descritor dela): do root, o arquivo
// novo ficava root:root 0600 — a Ana restaurava /home/ana/proj/relatorio.odt
// no lugar e não o abria, e o index.php restaurado em /var/www dava 403 ao
// nginx. O dono da pasta já pode criar arquivos nela; dar-lhe o arquivo não
// abre nada a mais.
//
// A cópia ao lado não herda setuid/setgid: seria um segundo binário
// privilegiado, com o conteúdo antigo. Só quem substitui o original os herda.
//
// O chown vem antes do chmod: no Linux o chown limpa S_ISUID e S_ISGID, mesmo
// feito pelo root, e o chmod antes dele deixava o binário setuid/setgid
// restaurado sem os bits, calado.
//
// O arquivo existente é lido pelo descritor da pasta (fstatat, sem seguir
// link). Era um os.Lstat do caminho em texto: durante o download a pessoa
// trocava a pasta por um link para /usr/bin, e o arquivo dela recebia o
// 04755 root:root do /usr/bin/passwd — root local.
func ajustarPermissoes(tmp *os.File, dir *pasta, baseName, finalName string) error {
	modo := modoDeArquivoNovo
	existente, err := dir.infoDe(baseName)
	if err == nil && existente.regular {
		modo = existente.modo
		if finalName != baseName {
			modo &^= os.ModeSetuid | os.ModeSetgid
		}
		if err := trocarDono(tmp, existente.uid, existente.gid); err != nil {
			return fmt.Errorf("chown %s: %w", tmp.Name(), err)
		}
	} else {
		uid, gid, err := dir.dono()
		if err != nil {
			return err
		}
		if err := trocarDono(tmp, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", tmp.Name(), err)
		}
	}
	if err := tmp.Chmod(modo); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	return nil
}

type downloadResult struct {
	bytes  int64
	sha256 string
}

// downloadObject baixa o objeto e calcula SHA-256.
//   - Arquivos < 100MB: GetObject streamed + io.MultiWriter
//   - Arquivos >= 100MB: s3manager.Downloader (multipart paralelo), depois
//     re-lê o tmp pra calcular sha256
//
// O segundo caminho lê o disco 2x; trade-off pra ter parallel multipart sem
// implementar próprio fan-out + reorder de chunks.
func downloadObject(
	ctx context.Context,
	s3c *s3.Client,
	item api.RestoreItem,
	tmpFile *os.File,
) (downloadResult, error) {
	if item.SourceSize >= MultipartThresholdBytes {
		return downloadMultipart(ctx, s3c, item, tmpFile)
	}
	return downloadStream(ctx, s3c, item, tmpFile)
}

func downloadStream(ctx context.Context, s3c *s3.Client, item api.RestoreItem, w io.Writer) (downloadResult, error) {
	in := &s3.GetObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		in.VersionId = &item.SourceVersionID
	}
	out, err := s3c.GetObject(ctx, in)
	if err != nil {
		return downloadResult{}, fmt.Errorf("s3 GetObject %s@%s: %w", item.SourceKey, item.SourceVersionID, err)
	}
	defer out.Body.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), out.Body)
	if err != nil {
		return downloadResult{}, fmt.Errorf("write tmp: %w", err)
	}
	return downloadResult{bytes: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

func downloadMultipart(ctx context.Context, s3c *s3.Client, item api.RestoreItem, tmpFile *os.File) (downloadResult, error) {
	in := &s3.GetObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		in.VersionId = &item.SourceVersionID
	}

	dl := manager.NewDownloader(s3c, func(d *manager.Downloader) {
		d.Concurrency = 4
		d.PartSize = 16 * 1024 * 1024 // 16 MiB
	})
	n, err := dl.Download(ctx, tmpFile, in)
	if err != nil {
		return downloadResult{}, fmt.Errorf("s3 multipart download %s@%s: %w", item.SourceKey, item.SourceVersionID, err)
	}

	// Re-lê pra hash (Downloader não permite TeeWriter)
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return downloadResult{}, fmt.Errorf("seek tmp: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, tmpFile); err != nil {
		return downloadResult{}, fmt.Errorf("hash tmp: %w", err)
	}
	return downloadResult{bytes: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// pedidoDeRestauracao monta o corpo do RestoreObject para a classe em questão.
//
// Vive separado para ter teste: a exceção do Intelligent-Tiering é uma regra de
// contrato de terceiro, e regra que o teste não consegue reprovar é regra sem
// teste.
//
// `Days: 7` é quanto a cópia quente fica disponível. Sete porque é folga para
// uma restauração grande terminar e porque **a OCI só aceita de 1 a 10** —
// medido contra o endpoint dela em 22/09, que devolve
// `InvalidArgument: Days parameter range is between 1 and 10`. A AWS aceita bem
// mais; sete serve aos dois.
//
// Intelligent-Tiering é a exceção: para as camadas de arquivo dele a AWS recusa
// `Days` e `GlacierJobParameters` — o pedido vai vazio e ela decide o tier.
//
// ⚠️ Essa exceção **não foi medida**. Um objeto só chega à camada de arquivo do
// Intelligent-Tiering depois de 90 dias sem acesso, e não há como forçar; em
// 22/09 confirmei apenas que, num objeto **não** arquivado, os dois formatos são
// recusados igualmente (`Restore is not allowed for the object's current storage
// class`), o que não distingue um do outro. Isto segue o contrato publicado pela
// AWS, que é melhor do que mandar parâmetros que ela documenta como inválidos —
// mas só um objeto de verdade arquivado fecha a questão.
func pedidoDeRestauracao(storageClass string, tier s3types.Tier) *s3types.RestoreRequest {
	if strings.Contains(storageClass, "INTELLIGENT_TIERING") {
		return &s3types.RestoreRequest{}
	}
	return &s3types.RestoreRequest{
		Days:                 aws32(7),
		GlacierJobParameters: &s3types.GlacierJobParameters{Tier: tier},
	}
}

// handleColdStorage é chamado quando GetObject retorna InvalidObjectState
// (objeto em GLACIER ou DEEP_ARCHIVE). Verifica via HeadObject se já há restore
// em andamento; se não, dispara RestoreObject.
//
// Tier Standard é usado por default (3-5h em GLACIER, 12h em DEEP_ARCHIVE).
// Para urgência, mudar pra Expedited (1-5min, mais caro) via item.metadata futuro.
func handleColdStorage(
	ctx context.Context,
	s3c *s3.Client,
	item api.RestoreItem,
	iose *s3types.InvalidObjectState,
) error {
	storageClass := ""
	if iose.StorageClass != "" {
		storageClass = string(iose.StorageClass)
	}

	// HeadObject pra checar x-amz-restore header
	headIn := &s3.HeadObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		headIn.VersionId = &item.SourceVersionID
	}
	head, herr := s3c.HeadObject(ctx, headIn)
	if herr == nil && head.Restore != nil {
		// "ongoing-request=\"true\"" → ainda warming
		// "ongoing-request=\"false\", expiry-date=..." → ready, mas GetObject falhou:
		//   improvável (raça), trate como warming-in-progress de qualquer forma
		if strings.Contains(*head.Restore, `ongoing-request="true"`) {
			return &ErrWarmingInProgress{
				StorageClass: storageClass,
				Bucket:       item.Bucket,
				Key:          item.SourceKey,
			}
		}
	}

	// Sem restore em andamento → dispara.
	tier := s3types.TierStandard
	restoreIn := &s3.RestoreObjectInput{
		Bucket:         &item.Bucket,
		Key:            &item.SourceKey,
		RestoreRequest: pedidoDeRestauracao(storageClass, tier),
	}
	if item.SourceVersionID != "" {
		restoreIn.VersionId = &item.SourceVersionID
	}
	if _, rerr := s3c.RestoreObject(ctx, restoreIn); rerr != nil {
		// Dois erros diferentes significam "não precisa fazer nada, espere":
		//
		//   RestoreAlreadyInProgress   já há uma restauração em andamento
		//   ObjectAlreadyInActiveTier  o objeto já está quente
		//
		// A primeira versão só tratava o segundo — e ainda dizia no comentário
		// que estava tratando o primeiro. O SDK **não tem tipo** para
		// RestoreAlreadyInProgress (não existe em s3/types/errors.go), então
		// `errors.As` nunca casava e o item caía no `default` do chamador, que
		// o marca como **falhado**. Basta a janela entre o nosso RestoreObject e
		// o cabeçalho `x-amz-restore` aparecer no HeadObject para uma espera
		// legítima virar falha.
		//
		// Casa por código, como `isObjectGone` já faz neste repositório.
		if jaEstaResolvendo(rerr) {
			return &ErrWarmingInProgress{
				StorageClass: storageClass,
				Bucket:       item.Bucket,
				Key:          item.SourceKey,
			}
		}
		return fmt.Errorf("s3 RestoreObject %s@%s: %w", item.SourceKey, item.SourceVersionID, rerr)
	}

	return &ErrWarmingRequested{
		PrevistoEm:   previsaoDeAquecimento(storageClass, tier),
		StorageClass: storageClass,
		Tier:         string(tier),
		Bucket:       item.Bucket,
		Key:          item.SourceKey,
	}
}

// aws32 retorna ponteiro pra int32 (helper, equivalente ao aws.Int32 do SDK).
func aws32(n int32) *int32 { return &n }

// splitDestFilename valida e separa o dest_filename em subdiretório relativo
// (POSIX, pode ser vazio) e nome-base. Rejeita path absoluto, backslash, "..",
// e segmentos vazios — o destino final fica sempre contido em dest_path.
func splitDestFilename(destFilename string) (subDir, baseName string, err error) {
	if destFilename == "" || strings.Contains(destFilename, "\\") || strings.HasPrefix(destFilename, "/") {
		return "", "", fmt.Errorf("dest_filename inválido: %q", destFilename)
	}
	segments := strings.Split(destFilename, "/")
	// A chave de um servidor Windows começa pela unidade (`C:/Users/…`);
	// dentro do destino ela vira pasta (`C\Users\…`) — `C:` não é nome válido
	// no meio de um caminho do Windows. O painel já faz isso; aqui é a segunda
	// camada.
	if len(segments) > 1 && caminho.Unidade(segments[0]) != "" && len(segments[0]) == 2 {
		segments[0] = segments[0][:1]
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("dest_filename inválido: %q", destFilename)
		}
	}
	baseName = segments[len(segments)-1]
	subDir = strings.Join(segments[:len(segments)-1], "/")
	return subDir, baseName, nil
}

// resolveConflict decide o nome final a usar dado o arquivo de destino, lido
// na pasta aberta sem seguir link (um link no nome final conta como existente
// e é substituído pelo rename, não seguido). Retorna nome vazio se a
// estratégia for "skip" e o arquivo já existir.
func resolveConflict(dir *pasta, filename, versionID, strategy string) (string, error) {
	if _, err := dir.infoDe(filename); errors.Is(err, os.ErrNotExist) {
		return filename, nil
	} else if err != nil {
		return "", err
	}

	switch strategy {
	case "overwrite":
		return filename, nil
	case "skip":
		return "", nil
	case "suffix-version", "":
		short := sufixoDeVersao(versionID)
		if short == "" {
			short = time.Now().UTC().Format("20060102T150405")
		}
		// Nome com sufixo já ocupado: incrementa o contador.
		for i := 0; ; i++ {
			candidate := nomeComVersao(filename, short, i)
			if _, err := dir.infoDe(candidate); errors.Is(err, os.ErrNotExist) {
				return candidate, nil
			}
			if i > 1000 {
				return "", fmt.Errorf("conflict resolution exaurido em %s", dir.caminho)
			}
		}
	default:
		return "", fmt.Errorf("conflict_strategy desconhecida: %q", strategy)
	}
}

// sufixoDeVersao é o pedaço do VersionId que vai no nome em suffix-version.
func sufixoDeVersao(versionID string) string {
	if len(versionID) > 8 {
		return versionID[:8]
	}
	return versionID
}

// nomeComVersao é o i-ésimo nome que suffix-version tenta:
// app.vXXXX.conf, app.vXXXX.1.conf, app.vXXXX.2.conf, ...
func nomeComVersao(filename, short string, i int) string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	if i == 0 {
		return fmt.Sprintf("%s.v%s%s", base, short, ext)
	}
	return fmt.Sprintf("%s.v%s.%d%s", base, short, i, ext)
}

// jaRestaurado diz se o destino já tem este item, inteiro: um arquivo com o
// tamanho e o sha256 esperados no nome que a estratégia de conflito usaria
// (o próprio nome e, em suffix-version, os nomes com o sufixo da versão).
// Sem sha256 no item não há como confirmar, e a restauração segue.
func jaRestaurado(dir *pasta, baseName string, item api.RestoreItem) (bool, error) {
	if item.SourceSha256 == "" {
		return false, nil
	}
	nomes := []string{baseName}
	if s := item.ConflictStrategy; s == "suffix-version" || s == "" {
		if short := sufixoDeVersao(item.SourceVersionID); short != "" {
			for i := 0; i <= 1000; i++ {
				n := nomeComVersao(baseName, short, i)
				if _, err := dir.infoDe(n); err != nil {
					break // o resolveConflict pararia aqui
				}
				nomes = append(nomes, n)
			}
		}
	}
	for _, n := range nomes {
		igual, err := mesmoConteudo(dir, n, item)
		if err != nil {
			return false, err
		}
		if igual {
			return true, nil
		}
	}
	return false, nil
}

// mesmoConteudo compara o arquivo nome da pasta aberta com o tamanho e o
// sha256 do item, sem seguir link. Só lê o arquivo se o tamanho bate; o que
// vale é o que o descritor aberto diz (entre a conferência e a abertura o nome
// pode ter virado outra coisa).
func mesmoConteudo(dir *pasta, nome string, item api.RestoreItem) (bool, error) {
	confere := func(st infoArquivo) bool {
		return st.regular && (item.SourceSize <= 0 || st.tamanho == item.SourceSize)
	}
	st, err := dir.infoDe(nome)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !confere(st) {
		return false, nil
	}
	f, st, err := dir.abrirLeitura(nome)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("abrindo %s: %w", nome, err)
	}
	defer f.Close()
	if !confere(st) {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("lendo %s: %w", f.Name(), err)
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), item.SourceSha256), nil
}

// jaEstaResolvendo diz se o erro do RestoreObject significa "espere", e não
// "falhou". Casa pelo código do erro porque o SDK não expõe tipo para
// RestoreAlreadyInProgress.
func jaEstaResolvendo(err error) bool {
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "RestoreAlreadyInProgress", "ObjectAlreadyInActiveTierError":
			return true
		}
	}
	// Rede de segurança: provedor compatível com S3 pode devolver o código só
	// no corpo, sem o tipo que o SDK reconhece.
	msg := err.Error()
	return strings.Contains(msg, "RestoreAlreadyInProgress") ||
		strings.Contains(msg, "ObjectAlreadyInActiveTier")
}

// previsaoDeAquecimento devolve o teto publicado pela AWS para a combinação
// classe × tier. Teto e não média de propósito: quem está restaurando backup
// prefere que a conta feche antes do prometido.
//
// Sem isto o cliente via um floco de neve e nenhum horizonte — no momento em
// que ele está mais nervoso, "vai demorar, não sei quanto" é a pior resposta.
func previsaoDeAquecimento(classe string, tier s3types.Tier) time.Time {
	agora := time.Now().UTC()
	switch tier {
	case s3types.TierExpedited:
		return agora.Add(5 * time.Minute)
	case s3types.TierBulk:
		if strings.Contains(classe, "DEEP_ARCHIVE") {
			return agora.Add(48 * time.Hour)
		}
		return agora.Add(12 * time.Hour)
	default: // Standard
		if strings.Contains(classe, "DEEP_ARCHIVE") {
			return agora.Add(12 * time.Hour)
		}
		// Classe vazia cai aqui, e é o caso da OCI: o HeadObject dela não
		// informa a classe de armazenamento. O arquivo da OCI sai em cerca de
		// uma hora, então as cinco prometidas são folga — que é o lado certo
		// para errar.
		return agora.Add(5 * time.Hour)
	}
}

// padraoTemporario é o nome do arquivo temporário da restauração. O nome final
// vai junto só para quem olhar a pasta entender o que é — e por isso pode ser
// cortado: com o prefixo e os dígitos aleatórios, um nome final perto do
// limite de 255 bytes dos sistemas de arquivos fazia o CreateTemp falhar
// (ENAMETOOLONG), e o arquivo, que caberia com o nome dele, não restaurava.
func padraoTemporario(finalName string) string {
	const prefixo = ".arkame-restore-*."
	// 255 menos o prefixo e até 20 dígitos aleatórios do CreateTemp.
	const maxSufixo = 255 - len(prefixo) - 20
	if len(finalName) > maxSufixo {
		corte := maxSufixo
		for corte > 0 && !utf8.RuneStart(finalName[corte]) {
			corte-- // não parte um caractere ao meio
		}
		finalName = finalName[:corte]
	}
	return prefixo + finalName
}
