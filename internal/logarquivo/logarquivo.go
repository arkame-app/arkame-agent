// Package logarquivo grava o log do agente num arquivo com rodízio por
// tamanho.
//
// Existe pelo serviço do Windows: o Service Control Manager não guarda a saída
// de erro do processo, e o log do agente ia para lugar nenhum — a dica
// "Get-EventLog" mostrada depois da instalação não achava nada, porque o
// agente nunca escreveu no Visualizador de Eventos. É o mesmo arranjo do
// launchd no macOS: um arquivo ao lado da configuração.
package logarquivo

import (
	"io"
	"os"
	"sync"
)

// TamanhoPadrao é o teto do arquivo antes do rodízio. Com o .1 anterior, o log
// ocupa no máximo o dobro disso.
const TamanhoPadrao = 10 * 1024 * 1024

// Arquivo é um io.Writer que, ao passar do teto, renomeia o atual para
// "<caminho>.1" (apagando o .1 anterior) e recomeça.
type Arquivo struct {
	mu      sync.Mutex
	caminho string
	teto    int64
	f       *os.File
	tamanho int64
	// adiarAte: depois de um rodízio que não deu certo de jeito nenhum, só
	// tenta de novo quando o arquivo chegar a este tamanho.
	adiarAte int64
}

// renomear é os.Rename; variável para os testes simularem o Windows com o
// arquivo aberto por outro processo.
var renomear = os.Rename

// Abrir abre (ou cria) o arquivo para acrescentar.
func Abrir(caminho string, teto int64) (*Arquivo, error) {
	a := &Arquivo{caminho: caminho, teto: teto}
	if err := a.abrir(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Arquivo) abrir() error {
	f, err := os.OpenFile(a.caminho, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	a.f, a.tamanho = f, st.Size()
	return nil
}

func (a *Arquivo) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		// Um rodízio anterior fechou o arquivo e não conseguiu reabrir.
		if err := a.abrir(); err != nil {
			return 0, err
		}
	}
	if a.tamanho > 0 && a.tamanho+int64(len(p)) > a.teto && a.tamanho >= a.adiarAte {
		if err := a.rodar(); err != nil {
			return 0, err
		}
	}
	n, err := a.f.Write(p)
	a.tamanho += int64(n)
	return n, err
}

// rodar passa o conteúdo atual para o .1 e recomeça o arquivo.
//
// No Windows o rename falha enquanto outro processo segura o arquivo sem
// FILE_SHARE_DELETE — é o caso do `Get-Content -Wait` de quem acompanha o
// log. Antes, cada Write tentava de novo, falhava de novo, e o arquivo
// crescia sem teto. Agora, sem rename, copia para o .1 e trunca o atual (o
// outro processo permite escrita, senão nós nem escreveríamos); se nem isso
// der, desiste por mais um quarto do teto em vez de insistir a cada linha.
func (a *Arquivo) rodar() error {
	// No Windows não se renomeia arquivo aberto: fecha antes.
	_ = a.f.Close()
	_ = os.Remove(a.caminho + ".1")
	errRen := renomear(a.caminho, a.caminho+".1")
	if err := a.abrir(); err != nil {
		a.f = nil
		return err
	}
	if errRen == nil {
		a.adiarAte = 0
		return nil
	}
	if err := a.copiarETruncar(); err != nil {
		a.adiarAte = a.tamanho + max(a.teto/4, 1)
		return nil
	}
	a.adiarAte = 0
	return nil
}

func (a *Arquivo) copiarETruncar() error {
	orig, err := os.Open(a.caminho)
	if err != nil {
		return err
	}
	defer orig.Close()
	dst, err := os.OpenFile(a.caminho+".1", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, orig); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	// Com O_APPEND, a próxima escrita vai para o novo fim (zero).
	if err := a.f.Truncate(0); err != nil {
		return err
	}
	a.tamanho = 0
	return nil
}

// Close fecha o arquivo.
func (a *Arquivo) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	return a.f.Close()
}
