// Command assinar-release assina o checksums.txt de um release com a chave
// Ed25519 de release, para o agente conferir antes de trocar o próprio
// programa (internal/atualizacao). Só biblioteca padrão; roda com `go run` na
// release (.goreleaser.yaml) e na mão de quem guarda a chave.
//
//	assinar-release gerar
//	    Gera um par de chaves e o imprime: a privada (uma linha, base64) e a
//	    pública (a que vai em internal/atualizacao/chaves.go). Nada é gravado.
//
//	assinar-release assinar <versão> <checksums.txt> [<saída>]
//	    Assina com a chave de ARKAME_RELEASE_ED25519_KEY e grava a assinatura
//	    (padrão: <checksums.txt>.ed25519).
//
//	assinar-release verificar <versão> <checksums.txt> [<assinatura>]
//	    Confere a assinatura contra as chaves embutidas neste build — as
//	    mesmas que o agente usa. Com ARKAME_RELEASE_ED25519_PUB, contra ela.
//
// O passo a passo de gerar, guardar e trocar a chave está em
// docs/RELEASE-ASSINATURA.md.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/arkame-app/agent/internal/atualizacao"
)

// Variáveis de ambiente: a chave privada (o seed de 32 bytes em base64) e,
// só para o `verificar`, uma chave pública no lugar das embutidas.
const (
	envChavePrivada = "ARKAME_RELEASE_ED25519_KEY"
	envChavePublica = "ARKAME_RELEASE_ED25519_PUB"
)

func main() {
	if err := rodar(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "assinar-release:", err)
		os.Exit(1)
	}
}

func rodar(args []string) error {
	if len(args) == 0 {
		return errors.New("uso: assinar-release gerar | assinar <versão> <checksums.txt> [<saída>] | verificar <versão> <checksums.txt> [<assinatura>]")
	}
	switch args[0] {
	case "gerar":
		return gerar()
	case "assinar":
		if len(args) < 3 || len(args) > 4 {
			return errors.New("uso: assinar-release assinar <versão> <checksums.txt> [<saída>]")
		}
		saida := args[2] + ".ed25519"
		if len(args) == 4 {
			saida = args[3]
		}
		return assinar(args[1], args[2], saida)
	case "verificar":
		if len(args) < 3 || len(args) > 4 {
			return errors.New("uso: assinar-release verificar <versão> <checksums.txt> [<assinatura>]")
		}
		assinatura := args[2] + ".ed25519"
		if len(args) == 4 {
			assinatura = args[3]
		}
		return verificar(args[1], args[2], assinatura)
	}
	return fmt.Errorf("comando desconhecido: %s", args[0])
}

func gerar() error {
	publica, privada, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Println("Chave privada (segredo " + envChavePrivada + " do environment `release`; guarde também uma cópia fora do GitHub):")
	fmt.Println(base64.StdEncoding.EncodeToString(privada.Seed()))
	fmt.Println()
	fmt.Println("Chave pública (constante chavePublicaAtual ou chavePublicaProxima em internal/atualizacao/chaves.go):")
	fmt.Println(base64.StdEncoding.EncodeToString(publica))
	return nil
}

// chavePrivada lê o seed do ambiente. Nunca de argumento: a linha de comando
// fica no histórico do shell e na lista de processos.
func chavePrivada() (ed25519.PrivateKey, error) {
	texto := strings.TrimSpace(os.Getenv(envChavePrivada))
	if texto == "" {
		return nil, fmt.Errorf("%s vazia: sem a chave de release não há o que assinar (docs/RELEASE-ASSINATURA.md)", envChavePrivada)
	}
	seed, err := base64.StdEncoding.DecodeString(texto)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s não é o seed Ed25519 de %d bytes em base64", envChavePrivada, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func assinar(versao, arquivo, saida string) error {
	v, err := atualizacao.LerVersao(versao)
	if err != nil {
		return err
	}
	chave, err := chavePrivada()
	if err != nil {
		return err
	}
	checksums, err := os.ReadFile(arquivo)
	if err != nil {
		return err
	}
	assinatura := atualizacao.Assinar(chave, v, checksums)
	// Confere o que acabou de assinar com as chaves que o agente desta mesma
	// árvore embute: assinar com uma chave que os agentes não conhecem
	// publicaria um release que nenhum deles aceita.
	publica := chave.Public().(ed25519.PublicKey)
	if embutidas := atualizacao.ChavesConfiaveis(); len(embutidas) > 0 {
		if err := atualizacao.VerificarAssinatura(embutidas, v, checksums, assinatura); err != nil {
			return fmt.Errorf("a chave de %s (pública %s) não é nenhuma das embutidas em internal/atualizacao/chaves.go: os agentes recusariam este release",
				envChavePrivada, base64.StdEncoding.EncodeToString(publica))
		}
	}
	if err := os.WriteFile(saida, assinatura, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "assinado %s (versão %s) → %s\n", arquivo, v, saida)
	return nil
}

func verificar(versao, arquivo, arquivoDaAssinatura string) error {
	v, err := atualizacao.LerVersao(versao)
	if err != nil {
		return err
	}
	checksums, err := os.ReadFile(arquivo)
	if err != nil {
		return err
	}
	assinatura, err := os.ReadFile(arquivoDaAssinatura)
	if err != nil {
		return err
	}
	chaves := atualizacao.ChavesConfiaveis()
	if texto := os.Getenv(envChavePublica); texto != "" {
		c, err := atualizacao.LerChavePublica(texto)
		if err != nil {
			return fmt.Errorf("%s: %w", envChavePublica, err)
		}
		chaves = []ed25519.PublicKey{c}
	}
	if err := atualizacao.VerificarAssinatura(chaves, v, checksums, assinatura); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "assinatura de %s confere (versão %s)\n", arquivo, v)
	return nil
}
