package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// bucketComObjeto serve o mesmo conteúdo a qualquer GetObject.
func bucketComObjeto(t *testing.T, conteudo []byte) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		_, _ = w.Write(conteudo)
	}))
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(srv.URL),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
}

func itemDe(dir, nome string, conteudo []byte, estrategia string) api.RestoreItem {
	h := sha256.Sum256(conteudo)
	return api.RestoreItem{
		ItemID: "i1", Bucket: "b", SourceKey: "data/a/" + nome, SourceVersionID: "v1",
		SourceSize: int64(len(conteudo)), SourceSha256: hex.EncodeToString(h[:]),
		DestPath: dir, DestFilename: nome, ConflictStrategy: estrategia,
	}
}

// Por cima de um arquivo existente: o modo dele fica. Antes, o CreateTemp
// (0600) ia junto no rename — um config lido por outro usuário deixava de ser
// legível depois da restauração.
func TestRestauracaoPreservaModoDoArquivoSubstituido(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a permissão é a ACL")
	}
	conteudo := []byte("conteúdo do backup")
	dir := t.TempDir()
	final := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(final, []byte("velho"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(final, 0o640); err != nil {
		t.Fatal(err)
	}
	item := itemDe(dir, "app.conf", conteudo, "overwrite")
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(final)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("modo depois da restauração = %v, queria 0640 (o do arquivo substituído)", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(final); string(b) != string(conteudo) {
		t.Fatal("conteúdo não foi restaurado")
	}
}

// Arquivo novo: 0600 (o modo original não está no índice, e o arquivo pode ser
// um segredo); e a data do backup, quando o painel a manda.
func TestRestauracaoArquivoNovoModoEData(t *testing.T) {
	conteudo := []byte("x")
	dir := t.TempDir()
	quando := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	item := itemDe(dir, "novo.txt", conteudo, "suffix-version")
	item.SourceModifiedAt = &quando
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "novo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("arquivo novo com modo %v, queria 0600", st.Mode().Perm())
	}
	if !st.ModTime().Equal(quando) {
		t.Fatalf("mtime = %v, queria %v (modified_at do backup)", st.ModTime(), quando)
	}

	// Sem a data, fica a de agora.
	item2 := itemDe(dir, "outro.txt", conteudo, "suffix-version")
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item2); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(filepath.Join(dir, "outro.txt"))
	if time.Since(st2.ModTime()) > time.Minute {
		t.Fatalf("sem modified_at, mtime deveria ser agora: %v", st2.ModTime())
	}
}

// Como root, o dono do arquivo substituído também fica.
func TestRestauracaoComoRootPreservaDono(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("exige root fora do Windows")
	}
	conteudo := []byte("y")
	dir := t.TempDir()
	final := filepath.Join(dir, "dono.txt")
	if err := os.WriteFile(final, []byte("velho"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(final, 1234, 5678); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, itemDe(dir, "dono.txt", conteudo, "overwrite")); err != nil {
		t.Fatal(err)
	}
	uid, gid := donoDe(t, final)
	if uid != 1234 || gid != 5678 {
		t.Fatalf("dono %d:%d, queria 1234:5678", uid, gid)
	}
}
