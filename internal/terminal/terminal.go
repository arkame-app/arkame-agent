// Package terminal lê respostas do operador direto do teclado.
//
// O instalador roda como `curl … | sh` e `irm … | iex`: a entrada padrão do
// processo é o próprio script, não o teclado. Por isso a leitura vai ao
// terminal de controle (/dev/tty; CONIN$ no Windows), e a senha é lida com o
// eco desligado.
package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrSemTerminal indica que não há quem responda: serviço, CI, container sem -it.
var ErrSemTerminal = errors.New("sem terminal para perguntar (rode o comando num terminal interativo)")

// Terminal é o teclado e a tela de quem está instalando.
type Terminal struct {
	in  *os.File
	out io.Writer
	r   *bufio.Reader
}

// Open abre o terminal de controle. Devolve ErrSemTerminal quando não há um.
func Open() (*Terminal, error) {
	f, err := openTTY()
	if err != nil {
		return nil, ErrSemTerminal
	}
	return &Terminal{in: f, out: os.Stderr, r: bufio.NewReader(f)}, nil
}

// Close libera o terminal.
func (t *Terminal) Close() error { return t.in.Close() }

// Pergunta mostra o rótulo e devolve a linha digitada, sem espaços nas pontas.
func (t *Terminal) Pergunta(rotulo string) (string, error) {
	fmt.Fprintf(t.out, "  %s: ", rotulo)
	return t.linha()
}

// Segredo é como Pergunta, sem mostrar o que é digitado.
func (t *Terminal) Segredo(rotulo string) (string, error) {
	fmt.Fprintf(t.out, "  %s: ", rotulo)
	restaurar, err := semEco(t.in)
	if err != nil {
		// Terminal que não deixa desligar o eco: melhor perguntar com eco do
		// que não conseguir instalar. Avisa antes.
		fmt.Fprint(t.out, "(o texto vai aparecer) ")
		return t.linha()
	}
	v, err := t.linha()
	restaurar()
	fmt.Fprintln(t.out)
	return v, err
}

func (t *Terminal) linha() (string, error) {
	s, err := t.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
