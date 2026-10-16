package localstore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Tetos do download: o zip do Linux tem ~197 MB e o programa ~267 MB. Acima
// disto, alguma coisa está errada (ou é um ataque de descompressão).
const (
	maxZip      = 400 << 20
	maxPrograma = 600 << 20
)

// clienteHTTP é o do download; os testes o trocam.
var clienteHTTP = &http.Client{Timeout: 30 * time.Minute}

// Baixar traz o RustFS desta plataforma para pasta, conferindo o SHA-256 do
// zip contra o embutido no agente, e devolve o caminho do programa.
//
// O zip vai para um arquivo temporário na própria pasta (o rename final fica
// no mesmo disco), e só o executável sai dele: o zip traz também o
// `rustfs-cli`, que o agente não usa. O programa novo só toma o lugar do
// antigo depois de inteiro em disco.
func Baixar(ctx context.Context, a Artefato, pasta string) (string, error) {
	if err := os.MkdirAll(pasta, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(pasta, ".rustfs-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL(), nil)
	if err != nil {
		return "", err
	}
	res, err := clienteHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("baixando o RustFS %s: %w", Versao, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("baixando o RustFS %s: HTTP %d", Versao, res.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(res.Body, maxZip+1))
	if err != nil {
		return "", fmt.Errorf("baixando o RustFS %s: %w", Versao, err)
	}
	if n > maxZip {
		return "", fmt.Errorf("o zip do RustFS passou de %d MB: recusado", maxZip>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return "", fmt.Errorf("o zip do RustFS não confere (SHA-256 %s, esperado %s): recusado", got, a.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}

	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return "", fmt.Errorf("abrindo o zip do RustFS: %w", err)
	}
	defer zr.Close()
	var entrada *zip.File
	for _, f := range zr.File {
		// Só o nome exato, na raiz do zip: nada de caminho com pasta ou `..`.
		if f.Name == a.Programa {
			entrada = f
			break
		}
	}
	if entrada == nil {
		return "", fmt.Errorf("o zip do RustFS não traz %s", a.Programa)
	}
	if entrada.UncompressedSize64 > maxPrograma {
		return "", fmt.Errorf("o programa do RustFS passou de %d MB: recusado", maxPrograma>>20)
	}
	destino := filepath.Join(pasta, a.Programa)
	novo := destino + ".novo"
	if err := extrair(entrada, novo); err != nil {
		os.Remove(novo)
		return "", err
	}
	if err := os.Rename(novo, destino); err != nil {
		os.Remove(novo)
		return "", err
	}
	return destino, nil
}

func extrair(f *zip.File, destino string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := os.OpenFile(destino, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, maxPrograma+1))
	if err == nil && n > maxPrograma {
		err = errors.New("o programa do RustFS passou do teto ao descompactar")
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
