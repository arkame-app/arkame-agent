package caminho

import (
	"path/filepath"
	"strings"
)

// As pastas que um plano vindo do MCP (assistente de IA) não pode ler: as
// mesmas do painel (`pastaSensivel`, apps/save/src/lib/mcp/para-o-modelo.ts).
// Lá a conferência é pelo texto; aqui, pelo caminho real, depois de seguir os
// links: `/srv/inocente -> /etc` passava pelo texto (0.5.5).
var sensiveisUnix = []string{
	"/root", "/etc/ssh", "/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-",
	"/etc/sudoers", "/etc/sudoers.d", "/etc/arkame", "/etc/ssl/private", "/etc/pki/tls/private",
	"/etc/letsencrypt", "/etc/kubernetes/pki", "/run/secrets", "/etc/master.passwd", "/proc",
	"/sysroot", "/ostree", "/.snapshots", "/system/volumes/data",
}

// Só a própria pasta ou dentro dela (não o que a contém): /var inteiro é backup comum.
var sensiveisSoDentro = []string{"/var/root", "/var/roothome", "/var/db/dslocal"}

var sensiveisWindows = []string{"/windows/system32/config", "/windows/ntds", "/etc/arkame"}

// cobre: p é s, está dentro de s, ou contém s (a raiz contém tudo).
func cobre(p, s string) bool {
	return p == "/" || p == s || strings.HasPrefix(p, s+"/") || strings.HasPrefix(s, p+"/")
}

// PastaSensivel devolve o caminho real da origem quando ele cai numa pasta
// sensível (ou a contém), e "" quando está liberada. `hostRoot` é a raiz do
// servidor (/host no Docker).
func PastaSensivel(hostRoot, origem string) string {
	if Windows {
		return sensivelNoWindows(FinalNoWindows(NoDisco(hostRoot, origem)))
	}
	return sensivelNoReal(hostRoot, Real(hostRoot, origem))
}

// CaminhoSensivel faz a mesma conferência sobre um caminho já resolvido
// (dentro de uma origem já conferida, ou o alvo de um link): é o que a cópia
// de um plano do MCP pula, para `/home` não levar `/home/x/.ssh`.
func CaminhoSensivel(hostRoot, real string) bool {
	if Windows {
		return sensivelNoWindows(real) != ""
	}
	return sensivelNoReal(hostRoot, real) != ""
}

func sensivelNoReal(hostRoot, real string) string {
	raiz := filepath.Clean(hostRoot)
	if raiz == "" || raiz == "." {
		raiz = "/"
	}
	rel, err := filepath.Rel(raiz, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return real
	}
	p := "/" + filepath.ToSlash(rel)
	if rel == "." {
		p = "/"
	}
	if sensivelNoUnix(p) {
		return p
	}
	return ""
}

func sensivelNoUnix(p string) bool {
	u := strings.ToLower(p)
	// No macOS, /etc e /var são links para /private/etc e /private/var, e o
	// caminho real vem com o prefixo: confere-se também sem ele. /private
	// inteiro contém /etc, como a raiz.
	if u == "/private" {
		return true
	}
	if resto, ok := strings.CutPrefix(u, "/private/"); ok && sensivelNoUnix("/"+resto) {
		return true
	}
	if pastaDeChave(u) {
		return true
	}
	for _, s := range sensiveisUnix {
		if cobre(u, s) {
			return true
		}
	}
	for _, s := range sensiveisSoDentro {
		if u == s || strings.HasPrefix(u, s+"/") {
			return true
		}
	}
	return false
}

func sensivelNoWindows(real string) string {
	u := strings.ToLower(strings.ReplaceAll(real, `\`, "/"))
	if pastaDeChave(u) {
		return real
	}
	unidade := ""
	if len(u) >= 2 && u[1] == ':' {
		unidade, u = u[:2], u[2:]
	}
	if u == "" {
		u = "/"
	}
	doSistema := unidade == "" || unidade == "c:"
	for _, s := range sensiveisWindows {
		if doSistema && cobre(u, s) || !doSistema && (u == s || strings.HasPrefix(u, s+"/")) {
			return real
		}
	}
	return ""
}

// pastaDeChave: as pastas de chaves do usuário (.ssh, .gnupg, .aws, .kube) e
// a configuração do agente de um usuário.
func pastaDeChave(u string) bool {
	for _, parte := range strings.Split(u, "/") {
		switch parte {
		case ".ssh", ".gnupg", ".aws", ".kube":
			return true
		}
	}
	return strings.Contains(u+"/", "/.config/arkame/")
}

// ResolvidoNoWindows diz se p é um caminho com unidade (C:\...) ou de
// compartilhamento (\\srv\...), e não um caminho de dispositivo
// (\\?\GLOBALROOT\...) que a conferência não sabe ler.
func ResolvidoNoWindows(p string) bool {
	if Unidade(p) != "" && len(p) >= 3 && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return strings.HasPrefix(p, `\\`) && !strings.HasPrefix(p, `\\?\`) && !strings.HasPrefix(p, `\\.\`)
}

// DoSistemaVivo diz se real (já resolvido) está em /proc ou /sys do servidor:
// pseudoarquivos do kernel, nunca dado de backup.
func DoSistemaVivo(hostRoot, real string) bool {
	raiz := filepath.Clean(hostRoot)
	if raiz == "" || raiz == "." {
		raiz = "/"
	}
	rel, err := filepath.Rel(raiz, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	p := "/" + filepath.ToSlash(rel)
	return p == "/proc" || p == "/sys" || strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/")
}
