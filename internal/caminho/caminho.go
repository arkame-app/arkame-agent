// Package caminho traduz os caminhos entre o painel, o disco e o bucket.
//
// No Linux e no macOS, o painel fala em caminhos absolutos (`/var/dados`),
// resolvidos dentro do HostRoot (`/host` no Docker). No Windows, o painel fala
// no formato do Windows (`C:\Users\Greyce`), e a unidade entra na chave do
// bucket (`C:/Users/Greyce/…`): sem ela, C:\dados e D:\dados cairiam no mesmo
// lugar, e a restauração não saberia para qual unidade voltar.
//
// Até 28/09 o agente tratava tudo como Linux: um plano com `C:\Users\…` virava
// `\C:\Users\…`, a leitura falhava calada e o backup saía "concluído" com zero
// arquivos (fundador, num Windows real).
//
// As funções com o sistema explícito são puras e testadas nos dois sistemas; as
// exportadas usam o sistema em que o agente roda.
package caminho

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Windows diz se o agente roda no Windows — a regra de caminho de todo o agente.
var Windows = runtime.GOOS == "windows"

// Unidade devolve "C:" quando o caminho começa por uma unidade do Windows.
func Unidade(p string) string {
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		return strings.ToUpper(p[:1]) + ":"
	}
	return ""
}

// NoDisco leva o caminho do plano ao caminho real nesta máquina.
func NoDisco(hostRoot, p string) string { return noDisco(Windows, hostRoot, p) }

func noDisco(windows bool, hostRoot, p string) string {
	if !windows {
		return dentroDoHost(hostRoot, p)
	}
	w := strings.ReplaceAll(p, "/", `\`)
	u := Unidade(w)
	if u == "" {
		// Plano antigo, escrito à moda Linux (`/Users/x`): era lido no disco
		// do sistema, e continua sendo.
		u = "C:"
	} else {
		w = w[2:]
	}
	w = `\` + strings.TrimLeft(w, `\`)
	w = strings.TrimRight(w, `\`)
	if w == "" {
		w = `\`
	}
	return u + w
}

// NaChave leva o caminho real ao caminho dentro do bucket, sempre com `/`.
func NaChave(hostRoot, abs string) string { return naChave(Windows, hostRoot, abs) }

func naChave(windows bool, hostRoot, abs string) string {
	if !windows {
		rel, err := filepath.Rel(hostRoot, abs)
		if err != nil {
			rel = strings.TrimPrefix(abs, hostRoot)
		}
		return filepath.ToSlash(rel)
	}
	u := Unidade(abs)
	resto := strings.Trim(strings.ReplaceAll(abs[len(u):], `\`, "/"), "/")
	if u == "" {
		return resto
	}
	return u + "/" + resto
}

// Destino valida o diretório de destino de uma restauração e o leva ao disco.
// No Windows aceita `C:\…`, `C:/…` e o formato antigo `/…` (disco do sistema).
func Destino(hostRoot, dir string) (string, error) { return destino(Windows, hostRoot, dir) }

func destino(windows bool, hostRoot, dir string) (string, error) {
	if !absoluto(windows, dir) {
		if windows {
			return "", fmt.Errorf(`destino deve ser absoluto, como C:\Restaurados: %q`, dir)
		}
		return "", fmt.Errorf("destino deve ser absoluto, como /restaurados: %q", dir)
	}
	if !windows {
		// A restauração grava no caminho de verdade: um link absoluto no
		// meio do destino não pode levar a gravação para dentro do container.
		return real(hostRoot, dir), nil
	}
	return noDisco(windows, hostRoot, dir), nil
}

// Absoluto diz se o caminho do painel é absoluto neste sistema: `C:\…` (ou o
// formato antigo `/…`) no Windows, `/…` no resto.
func Absoluto(p string) bool { return absoluto(Windows, p) }

func absoluto(windows bool, p string) bool {
	if windows {
		return Unidade(p) != "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
	}
	return strings.HasPrefix(p, "/")
}

// dentroDoHost junta ao HostRoot sem deixar `..` sair dele: no Docker, o
// servidor está em /host, e `/../..` levaria ao container.
func dentroDoHost(hostRoot, p string) string {
	return filepath.Join(hostRoot, filepath.Clean("/"+p))
}

// Raiz diz se o pedido do painel é a raiz do navegador de pastas — no Windows,
// a lista de unidades.
func Raiz(p string) bool {
	return p == "" || p == "/" || p == `\`
}
