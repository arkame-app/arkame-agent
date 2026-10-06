package atualizacao

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lugar são os arquivos de uma troca, todos na pasta do programa: a troca é
// por rename, que só é atômico dentro da mesma pasta (e do mesmo sistema de
// arquivos).
type Lugar struct {
	// Atual é o programa que o serviço executa.
	Atual string
	// Novo é para onde o programa baixado vai, antes de ser conferido.
	Novo string
	// Anterior é o programa que estava no lugar antes da troca: é o que volta
	// se o novo não provar que funciona.
	Anterior string
	// Estado é o registro da troca (estado.go).
	Estado string
	// Trava impede duas trocas ao mesmo tempo: dois agentes na mesma máquina
	// (um por credencial de bucket) dividem o programa.
	Trava string
	// base é o prefixo dos arquivos por versão (arquivosDaVersao).
	base string
}

// arquivosDaVersao são o checksums.txt do release de uma versão e a
// assinatura dele, guardados para o lançador do Docker conferir o programa do
// volume de novo a cada partida, sem rede.
//
// O nome leva a versão: com um nome só, a troca seguinte gravaria o
// checksums.txt da versão nova por cima antes de trocar o programa — e uma
// troca que falhasse ali deixaria o programa bom sem o checksums.txt dele, e o
// lançador o abandonaria.
func (l Lugar) arquivosDaVersao(v Versao) (checksums, assinatura string) {
	checksums = l.base + ".checksums-" + v.String() + ".txt"
	return checksums, checksums + ".ed25519"
}

// limparOutrasVersoes apaga os checksums.txt guardados de versões que não são
// a que ficou no lugar.
func (l Lugar) limparOutrasVersoes(fica Versao) {
	guardados, _ := filepath.Glob(l.base + ".checksums-*.txt*")
	c, a := l.arquivosDaVersao(fica)
	for _, g := range guardados {
		if g != c && g != a {
			_ = os.Remove(g)
		}
	}
}

// LugarDe monta os caminhos ao lado do programa. No Windows a extensão fica
// no fim (arkame-agent.novo.exe): o programa novo é executado no auto-teste.
func LugarDe(programa string) Lugar {
	dir, base := filepath.Split(programa)
	ext := ""
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		ext = base[len(base)-4:]
		base = base[:len(base)-4]
	}
	em := func(sufixo string) string { return filepath.Join(dir, base+sufixo) }
	return Lugar{
		Atual:    programa,
		Novo:     em(".novo" + ext),
		Anterior: em(".anterior" + ext),
		Estado:   em(".atualizacao.json"),
		Trava:    em(".atualizando"),
		base:     em(""),
	}
}

// validadeDaTrava é depois de quanto tempo uma trava é de um processo que
// morreu no meio da troca. A troca inteira tem prazo menor (PrazoDaTroca).
const validadeDaTrava = 30 * time.Minute

// travar cria a trava da troca. ok é false quando outra troca está em curso.
// Uma trava mais velha que validadeDaTrava é de um processo que morreu: sai e
// a trava é tentada de novo.
func (l Lugar) travar() (destravar func(), ok bool, err error) {
	for tentativa := 0; tentativa < 2; tentativa++ {
		f, err := os.OpenFile(l.Trava, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(l.Trava) }, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		fi, serr := os.Stat(l.Trava)
		if serr != nil || time.Since(fi.ModTime()) < validadeDaTrava {
			return nil, false, nil
		}
		_ = os.Remove(l.Trava)
	}
	return nil, false, nil
}

// PastaGravavel diz se este processo consegue criar arquivo na pasta do
// programa — sem isso não há como trocar. É o caso do serviço do sistema no
// Linux instalado antes da atualização automática: o ProtectSystem=full da
// unit deixa /usr só para leitura, e a unit nova libera a pasta do programa
// (internal/service/systemd.go).
func (l Lugar) PastaGravavel() bool {
	f, err := os.CreateTemp(filepath.Dir(l.Atual), ".arkame-agent.gravavel-*")
	if err != nil {
		return false
	}
	nome := f.Name()
	_ = f.Close()
	return os.Remove(nome) == nil
}
