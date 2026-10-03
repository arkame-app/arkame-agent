package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// releasesDoAgente é de onde o install.ps1 e o install.sh baixam: o
// checksums.txt de cada release cobre os pacotes e os programas avulsos.
const releasesDoAgente = "https://github.com/arkame-app/arkame-agent/releases/download"

var (
	// errChecksumDiferente: o programa não é o publicado no release.
	errChecksumDiferente = errors.New("o checksum do programa baixado não confere com o do release")
	// errSemConferencia: não deu para conferir (rede, release ou linha
	// ausente, versão de desenvolvimento) — não diz nada sobre o programa.
	errSemConferencia = errors.New("não deu para conferir o checksum")
)

// nomeNoReleaseDeste é o nome do programa avulso deste sistema no release
// (arkame-agent_windows_amd64.exe), o mesmo que get.arkame.app/agente.exe
// entrega.
func nomeNoReleaseDeste() string {
	nome := "arkame-agent_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		nome += ".exe"
	}
	return nome
}

// conferirPrograma baixa o checksums.txt do release da versão e compara o
// SHA-256 de caminho com a linha de nome.
func conferirPrograma(ctx context.Context, cliente *http.Client, base, versao, nome, caminho string) error {
	if versao == "" || versao == "dev" {
		return fmt.Errorf("%w: versão de desenvolvimento (%q), sem release publicado", errSemConferencia, versao)
	}
	tag := versao
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	url := strings.TrimRight(base, "/") + "/" + tag + "/checksums.txt"
	esperado, err := checksumNoRelease(ctx, cliente, url, nome)
	if err != nil {
		return fmt.Errorf("%w: %v", errSemConferencia, err)
	}
	f, err := os.Open(caminho)
	if err != nil {
		return fmt.Errorf("%w: %v", errSemConferencia, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("%w: lendo %s: %v", errSemConferencia, caminho, err)
	}
	obtido := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(esperado, obtido) {
		return fmt.Errorf("%w (%s %s)\n     esperado: %s\n     obtido:   %s", errChecksumDiferente, nome, tag, esperado, obtido)
	}
	return nil
}

func checksumNoRelease(ctx context.Context, cliente *http.Client, url, nome string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := cliente.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s respondeu %s", url, resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		if len(campos) == 2 && strings.TrimPrefix(campos[1], "*") == nome {
			return campos[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("lendo %s: %v", url, err)
	}
	return "", fmt.Errorf("%s não está em %s", nome, url)
}

// verificarBaixado é a decisão do setup: checksum diferente aborta sempre;
// sem como conferir, aborta também, a menos que pular (--skip-checksum).
func verificarBaixado(ctx context.Context, cliente *http.Client, base, versao, nome, exe string, pular bool, saida io.Writer) error {
	err := conferirPrograma(ctx, cliente, base, versao, nome, exe)
	switch {
	case err == nil:
		fmt.Fprintln(saida, "  ✓ Checksum conferido")
		return nil
	case errors.Is(err, errChecksumDiferente):
		return fmt.Errorf("%w\n     Não instalei nada: baixe de novo pelo comando do painel", err)
	case pular:
		fmt.Fprintf(saida, "  ! seguindo sem conferir o checksum (--skip-checksum): %v\n", err)
		return nil
	default:
		return fmt.Errorf("%w\n     Não instalei nada. Confira a conexão com github.com e rode o comando de novo; para instalar sem conferir, acrescente --skip-checksum", err)
	}
}

// clienteDoChecksum baixa o checksums.txt (com redirecionamento para o
// armazenamento do GitHub e o proxy do ambiente).
var clienteDoChecksum = &http.Client{Timeout: 30 * time.Second}
