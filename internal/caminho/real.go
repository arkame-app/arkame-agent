package caminho

import (
	"os"
	"path/filepath"
	"strings"
)

// maxLinks limita a cadeia de links seguidos, como o próprio kernel (ELOOP).
const maxLinks = 40

// Real leva o caminho do plano ao arquivo de verdade dentro do HostRoot,
// seguindo os links simbólicos como o servidor os seguiria.
//
// No Docker o servidor está em /host, e o sistema do container resolveria um
// link pelo lado errado: `/home -> /var/home` apontaria para o /var/home do
// container. Aqui um link absoluto recomeça na raiz do servidor (o HostRoot),
// um relativo continua da pasta onde está, e `..` não sobe acima do HostRoot.
// Em distribuições como Fedora Atomic (Silverblue, Bluefin), /home, /opt e
// /root são links — antes disso o navegador de pastas os mostrava como
// arquivos, e um plano com /home não teria o que copiar (fundador, 02/10).
//
// No Windows não há o que seguir: devolve o mesmo que NoDisco.
func Real(hostRoot, p string) string {
	if Windows {
		return NoDisco(hostRoot, p)
	}
	return real(hostRoot, p)
}

// RealNoDisco é o Real de um caminho que já está no disco (com o HostRoot na
// frente): a pasta final de uma restauração, montada do destino mais o
// caminho original do arquivo, pode ter um link no meio.
func RealNoDisco(hostRoot, disco string) string {
	if Windows {
		return disco
	}
	raiz := filepath.Clean(hostRoot)
	if raiz == "/" || raiz == "." || raiz == "" {
		return real("/", disco)
	}
	return real(raiz, strings.TrimPrefix(disco, raiz))
}

func real(hostRoot, p string) string {
	raiz := filepath.Clean(hostRoot)
	if raiz == "" || raiz == "." {
		raiz = "/"
	}
	pendentes := partes(p)
	atual := "/" // caminho dentro do servidor, sem a raiz
	seguidos := 0
	for len(pendentes) > 0 {
		c := pendentes[0]
		pendentes = pendentes[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			atual = filepath.Dir(atual)
			continue
		}
		prox := filepath.Join(atual, c)
		noDisco := filepath.Join(raiz, prox)
		info, err := os.Lstat(noDisco)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || seguidos >= maxLinks {
			// Não existe (o erro aparece adiante, no uso), não é link, ou a
			// cadeia é longa demais: segue pelo nome como está.
			atual = prox
			continue
		}
		alvo, err := os.Readlink(noDisco)
		if err != nil {
			atual = prox
			continue
		}
		seguidos++
		if filepath.IsAbs(alvo) {
			atual = "/"
		}
		pendentes = append(partes(alvo), pendentes...)
	}
	return filepath.Join(raiz, atual)
}

func partes(p string) []string {
	return strings.Split(strings.Trim(filepath.ToSlash(p), "/"), "/")
}
