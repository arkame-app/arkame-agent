package atualizacao

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
)

// trocaPorRenome: no Windows o programa em uso não pode ser substituído, mas
// pode ser renomeado — o atual sai do caminho e o novo entra. Fora dele o novo
// entra por cima do atual num rename só. Variável para os testes exercitarem
// os dois caminhos no mesmo sistema.
var trocaPorRenome = runtime.GOOS == "windows"

// trocar põe o programa novo no lugar do atual e guarda o atual como
// anterior.
//
// Fora do Windows o caminho do programa nunca fica vazio: o anterior é um
// segundo nome (link) do atual, e o novo entra por rename, atômico. Com o
// renomeia-e-renomeia, uma queda entre os dois passos deixava o serviço sem
// programa para subir. No Windows não há alternativa (o .exe em uso não aceita
// ser substituído), e a janela é de um rename; se o segundo falha, o primeiro
// é desfeito.
//
// Sem programa atual (a primeira troca no volume do Docker), o novo só entra.
func (l Lugar) trocar() error {
	l.limparRestos()
	if _, err := os.Lstat(l.Atual); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(l.Novo, l.Atual); err != nil {
			return err
		}
		segredo.SincronizarPasta(filepath.Dir(l.Atual))
		return nil
	} else if err != nil {
		return err
	}
	if err := tirarDoCaminho(l.Anterior); err != nil {
		return fmt.Errorf("tirando o programa anterior da troca passada: %w", err)
	}
	if !trocaPorRenome {
		if err := os.Link(l.Atual, l.Anterior); err == nil {
			if err := os.Rename(l.Novo, l.Atual); err != nil {
				_ = os.Remove(l.Anterior)
				return err
			}
			segredo.SincronizarPasta(filepath.Dir(l.Atual))
			return nil
		}
		// Sistema de arquivos sem link (alguns de rede): o caminho do Windows.
	}
	// Daqui até o segundo rename o caminho do programa fica vazio. Tudo o que
	// podia falhar ou demorar já aconteceu (o registro está no disco, o novo
	// está conferido e sincronizado, os restos saíram): sobram dois renames
	// seguidos. O segundo é tentado de novo antes de desistir — no Windows um
	// antivírus que ainda lê o arquivo recém-baixado o segura por um instante
	// —, e só então o primeiro é desfeito. Uma queda de energia exatamente
	// aqui deixa o `.anterior` e o `.novo` inteiros ao lado: ver Recuperar.
	if err := os.Rename(l.Atual, l.Anterior); err != nil {
		return err
	}
	var err error
	for tentativa := 0; tentativa < tentativasDoSegundoRename; tentativa++ {
		if tentativa > 0 {
			time.Sleep(pausaEntreRenames)
		}
		if err = renomear(l.Novo, l.Atual); err == nil {
			segredo.SincronizarPasta(filepath.Dir(l.Atual))
			return nil
		}
	}
	_ = os.Rename(l.Anterior, l.Atual) // devolve o que estava lá
	return err
}

// renomear é o os.Rename; os testes o trocam para simular o arquivo preso.
var renomear = os.Rename

const tentativasDoSegundoRename = 5

var pausaEntreRenames = 200 * time.Millisecond

// Recuperar conserta a troca que parou entre os dois renames (queda de
// energia, processo morto): o caminho do programa está vazio, e ao lado
// ficaram o `.novo` e o `.anterior`. Com o registro da troca em prova, entra o
// `.novo` se ele confere com o SHA-256 registrado; senão volta o `.anterior`,
// se confere com o dele. Sem registro, ou sem arquivo que confira, não mexe em
// nada. Devolve o que fez ("", "novo" ou "anterior").
//
// Só adianta para quem ainda consegue rodar: outro agente da máquina que
// divide o programa (o daemon dele chama isto a cada pedido de troca), o
// comando `update`, ou alguém que execute o `.anterior` à mão (`run` chama
// isto antes de tudo). O serviço cujo programa sumiu não sobe sozinho — o
// gerenciador de serviço não tem o que executar; ver docs/RELEASE-ASSINATURA.md.
func Recuperar(programa string) string {
	l := LugarDe(programa)
	if _, err := os.Lstat(l.Atual); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	// Com a trava: quem está no meio de uma troca também passa por um instante
	// sem programa no lugar, e não é para ser "consertado".
	destravar, ok, err := l.travar()
	if err != nil || !ok {
		return ""
	}
	defer destravar()
	return l.recuperar()
}

func (l Lugar) recuperar() string {
	if _, err := os.Lstat(l.Atual); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	e, ok := l.lerEstado()
	if !ok || e.Situacao != SituacaoEmProva || e.PeloLancador {
		return ""
	}
	confere := func(caminho, resumo string) bool {
		if resumo == "" || arquivoComum(caminho) != nil {
			return false
		}
		r, err := resumoDoArquivo(caminho)
		return err == nil && r == resumo
	}
	if confere(l.Novo, e.Sha256) {
		if os.Rename(l.Novo, l.Atual) == nil {
			segredo.SincronizarPasta(filepath.Dir(l.Atual))
			return "novo"
		}
		return ""
	}
	if confere(l.Anterior, e.Sha256Anterior) {
		if os.Rename(l.Anterior, l.Atual) != nil {
			return ""
		}
		segredo.SincronizarPasta(filepath.Dir(l.Atual))
		e.Situacao = SituacaoRevertida
		e.Motivo = MotivoTrocaIncomplet
		_ = l.gravarEstado(e)
		return "anterior"
	}
	return ""
}

// ProgramaDoAnterior: se caminho é o `.anterior` de um programa, devolve o
// programa. É como o `.anterior` executado à mão sabe o que recuperar.
func ProgramaDoAnterior(caminho string) (string, bool) {
	dir, base := filepath.Split(caminho)
	ext := ""
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		ext = base[len(base)-4:]
		base = base[:len(base)-4]
	}
	nome, ok := strings.CutSuffix(base, ".anterior")
	if !ok || nome == "" {
		return "", false
	}
	return filepath.Join(dir, nome+ext), true
}

// ErrSemAnterior: não há programa anterior utilizável para voltar — não
// existe, não é um arquivo comum, ou não é o que a troca guardou.
var ErrSemAnterior = errors.New("não há programa anterior utilizável ao lado do atual para voltar")

// reverter devolve o programa anterior ao lugar. O que estava no lugar (o
// novo, que não provou que funciona) sai: fora do Windows o rename o
// substitui; no Windows, em uso, ele é renomeado e apagado quando der.
//
// O `.anterior` só volta se é um arquivo comum (um link posto ali iria para o
// lugar do programa) e, quando a troca registrou o resumo dele, se confere.
func (l Lugar) reverter(resumoDoAnterior string) error {
	if err := arquivoComum(l.Anterior); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrSemAnterior
		}
		if _, serr := os.Lstat(l.Anterior); serr != nil {
			return serr
		}
		return fmt.Errorf("%w: %v", ErrSemAnterior, err)
	}
	if resumoDoAnterior != "" {
		noDisco, err := resumoDoArquivo(l.Anterior)
		if err != nil {
			return err
		}
		if noDisco != resumoDoAnterior {
			return fmt.Errorf("%w: o SHA-256 dele (%s) não é o do programa que saiu na troca (%s)", ErrSemAnterior, noDisco, resumoDoAnterior)
		}
	}
	if !trocaPorRenome {
		if err := os.Rename(l.Anterior, l.Atual); err != nil {
			return err
		}
		segredo.SincronizarPasta(filepath.Dir(l.Atual))
		return nil
	}
	falhou := l.Atual + ".falhou-" + sufixoAleatorio()
	if err := os.Rename(l.Atual, falhou); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(l.Anterior, l.Atual); err != nil {
		_ = os.Rename(falhou, l.Atual)
		return err
	}
	_ = os.Remove(falhou) // em uso, fica para a limpeza da próxima troca
	return nil
}

// tirarDoCaminho apaga o arquivo; se ele está em uso (no Windows, um segundo
// agente ainda roda o programa de duas trocas atrás), só o renomeia.
func tirarDoCaminho(caminho string) error {
	err := os.Remove(caminho)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return os.Rename(caminho, caminho+".falhou-"+sufixoAleatorio())
}

// limparRestos tira o que trocas anteriores deixaram por estar em uso.
func (l Lugar) limparRestos() {
	dir := filepath.Dir(l.Atual)
	entradas, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, base := range []string{filepath.Base(l.Atual), filepath.Base(l.Anterior)} {
		for _, e := range entradas {
			if strings.HasPrefix(e.Name(), base+".falhou-") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

func sufixoAleatorio() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // nunca falha (crypto/rand, Go 1.24+)
	return hex.EncodeToString(b)
}
