// Package localstore cuida da cópia local: um RustFS (S3 compatível, código
// aberto) na máquina do cliente, instalado e mantido de pé pelo agente.
//
// "Local primeiro, nuvem depois": o plano local grava aqui, na velocidade do
// disco, e um plano ligado leva os mesmos arquivos à nuvem. A restauração do
// dia a dia sai da cópia local; a nuvem é a cópia fora do alcance de um ataque
// à rede. Por isso o RustFS só escuta em 127.0.0.1: a cópia local não é um
// serviço da rede, é do agente.
//
// Quem roda o RustFS é um processo do próprio agente (o irmão do bucket local,
// `--service-name arkame-agent-local`), como filho supervisionado: sobe junto,
// volta se cair, para quando o serviço para. O `rustfs.exe` não fala com o
// gerenciador de serviços do Windows — instalado como serviço próprio, o
// Windows o mataria em 30 segundos.
package localstore

import (
	"fmt"
	"runtime"
)

// Versao é a versão do RustFS que o agente instala. Fixada: a cópia local não
// muda de programa sem uma versão nova do agente, que traz o SHA-256 novo.
const Versao = "1.0.1"

// Artefato é o arquivo da release do RustFS para uma plataforma.
type Artefato struct {
	// Arquivo é o nome do zip na release.
	Arquivo string
	// SHA256 é o hash do zip, copiado do SHA256SUMS da release (conferido em
	// 16/10/2026). O agente recusa o download que não bate.
	SHA256 string
	// Programa é o nome do executável dentro do zip.
	Programa string
}

// artefatos por GOOS/GOARCH. Linux com musl: estático, roda em qualquer
// distribuição. Sem build para Windows ARM nem macOS Intel na release.
var artefatos = map[string]Artefato{
	"linux/amd64":   {Arquivo: "rustfs-linux-x86_64-musl-v1.0.1.zip", SHA256: "a834096dafa1f1a55825a2cdaf49d006a193978d344f2d508c2be475133738a3", Programa: "rustfs"},
	"linux/arm64":   {Arquivo: "rustfs-linux-aarch64-musl-v1.0.1.zip", SHA256: "d2533e293204597416cb8d30790ea35df14cb4521633fa3574c64333141bafdf", Programa: "rustfs"},
	"darwin/arm64":  {Arquivo: "rustfs-macos-aarch64-v1.0.1.zip", SHA256: "18aac7101c3484b98f93de64a0609aaa8e02aba072ba7927172c4b2eae7d4843", Programa: "rustfs"},
	"windows/amd64": {Arquivo: "rustfs-windows-x86_64-v1.0.1.zip", SHA256: "624259622524255d0000131e095f37a11299fd76926aabfd8f831a9dbe6a39f7", Programa: "rustfs.exe"},
}

// baseDaRelease é de onde o zip vem; os testes a trocam.
var baseDaRelease = "https://github.com/rustfs/rustfs/releases/download/" + Versao + "/"

// ArtefatoDaPlataforma devolve o artefato desta máquina, ou erro quando não há
// build do RustFS para ela.
func ArtefatoDaPlataforma() (Artefato, error) {
	return artefatoDe(runtime.GOOS, runtime.GOARCH)
}

func artefatoDe(goos, goarch string) (Artefato, error) {
	a, ok := artefatos[goos+"/"+goarch]
	if !ok {
		return Artefato{}, fmt.Errorf("não há cópia local para %s/%s (o RustFS %s não tem build para esta plataforma)", goos, goarch, Versao)
	}
	return a, nil
}

// URL do zip do artefato.
func (a Artefato) URL() string { return baseDaRelease + a.Arquivo }
