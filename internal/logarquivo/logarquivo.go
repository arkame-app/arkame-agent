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
}

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
	if a.tamanho > 0 && a.tamanho+int64(len(p)) > a.teto {
		// No Windows não se renomeia arquivo aberto: fecha antes.
		_ = a.f.Close()
		_ = os.Remove(a.caminho + ".1")
		_ = os.Rename(a.caminho, a.caminho+".1")
		if err := a.abrir(); err != nil {
			return 0, err
		}
	}
	n, err := a.f.Write(p)
	a.tamanho += int64(n)
	return n, err
}

// Close fecha o arquivo.
func (a *Arquivo) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}
