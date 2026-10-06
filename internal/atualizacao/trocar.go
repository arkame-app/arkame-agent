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
	if err := os.Rename(l.Atual, l.Anterior); err != nil {
		return err
	}
	if err := os.Rename(l.Novo, l.Atual); err != nil {
		_ = os.Rename(l.Anterior, l.Atual) // devolve o que estava lá
		return err
	}
	segredo.SincronizarPasta(filepath.Dir(l.Atual))
	return nil
}

// ErrSemAnterior: não há programa anterior para voltar.
var ErrSemAnterior = errors.New("não há programa anterior ao lado do atual para voltar")

// reverter devolve o programa anterior ao lugar. O que estava no lugar (o
// novo, que não provou que funciona) sai: fora do Windows o rename o
// substitui; no Windows, em uso, ele é renomeado e apagado quando der.
func (l Lugar) reverter() error {
	if _, err := os.Lstat(l.Anterior); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrSemAnterior
		}
		return err
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
