// Package hooks executa os comandos que o plano define para antes e depois do
// backup.
//
// Existe pelo caso que a vitrine promete há tempos: `pg_dump` antes de copiar.
// Backup do arquivo de um banco aberto é backup de algo que não restaura — o
// arquivo muda enquanto está sendo lido. Sem um comando que gere o dump antes,
// o produto entrega exatamente esse problema a quem tem banco de dados, que é
// boa parte de quem procura backup.
//
// Isto executa comando arbitrário na máquina do cliente, e é por isso que o
// pacote é pequeno e as regras são explícitas:
//
//   - O comando roda pelo shell, porque é assim que a pessoa escreveu:
//     `pg_dump ... > /tmp/dump.sql` só funciona com redirecionamento.
//   - Tem prazo. Um comando pendurado seguraria o agendamento inteiro do
//     servidor, e o próximo backup nunca começaria.
//   - A saída volta truncada. Ela serve para diagnóstico no painel, e um
//     comando verboso não pode virar um megabyte por sessão no banco.
//   - Falha do comando de antes aborta o backup. Quem decide isso é o chamador,
//     mas o erro sai daqui distinguível.
package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// MaxOutputBytes limita o que volta ao painel por comando.
//
// Oito mil bytes cobrem a mensagem de erro de qualquer ferramenta de banco com
// folga. O que passa disso é ruído de progresso, e ruído de progresso
// multiplicado por uma sessão a cada hora vira tabela grande sem ninguém
// perceber.
const MaxOutputBytes = 8 * 1024

// Result descreve o que aconteceu com um comando.
type Result struct {
	Ran      bool
	ExitCode int
	Output   string
	Duration time.Duration
	// TimedOut distingue "o comando falhou" de "o comando não terminou" — são
	// problemas diferentes e o texto na tela precisa dizer qual foi.
	TimedOut bool
}

// Run executa um comando com prazo e devolve a saída combinada, truncada.
//
// Devolve Ran=false, sem erro, quando não há comando: plano sem hook é o caso
// normal, não uma condição de erro.
func Run(ctx context.Context, comando string, prazo time.Duration) (Result, error) {
	comando = strings.TrimSpace(comando)
	if comando == "" {
		return Result{Ran: false}, nil
	}
	if prazo <= 0 {
		prazo = 15 * time.Minute
	}

	ctx, cancelar := context.WithTimeout(ctx, prazo)
	defer cancelar()

	// Sem `CommandContext`, de propósito.
	//
	// Ele mata só o processo direto — o `/bin/sh`. Um hook de verdade quase
	// nunca é um processo só: `pg_dump … | gzip > arquivo` são três, e os que
	// sobram continuam segurando aberto o pipe de onde lemos a saída. Como o
	// `Stdout` aqui é um buffer e não um arquivo, o `exec` monta esse pipe e o
	// `Wait` espera as goroutines de cópia terminarem — ou seja, espera os
	// netos, mesmo depois de o shell morrer.
	//
	// O sintoma foi um teste vermelho no `main` desde 08/09 que ninguém viu:
	// `TestPrazoInterrompe` pedia 300 ms e levava 5 s, exatamente a duração do
	// `sleep 5` que deveria ter sido interrompido. Passava nesta máquina porque
	// aqui o `/bin/sh` faz `exec` e o neto não existe; no Ubuntu do runner, não.
	// O nome do teste já dizia a consequência para o cliente: o agendamento
	// inteiro do servidor ficaria preso, e o próximo backup nunca começaria.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", comando)
	} else {
		cmd = exec.Command("/bin/sh", "-c", comando)
	}
	grupoProprio(cmd)

	buf := &bufferSeguro{}
	cmd.Stdout = buf
	cmd.Stderr = buf

	inicio := time.Now()
	err := executarComPrazo(ctx, cmd)
	r := Result{
		Ran:      true,
		Duration: time.Since(inicio),
		Output:   truncar(buf.String()),
		TimedOut: ctx.Err() == context.DeadlineExceeded,
	}

	if err == nil {
		return r, nil
	}
	var saida *exec.ExitError
	if ok := asExitError(err, &saida); ok {
		r.ExitCode = saida.ExitCode()
	} else {
		r.ExitCode = -1
	}
	if r.TimedOut {
		return r, fmt.Errorf("comando excedeu o prazo de %s", prazo)
	}
	return r, fmt.Errorf("comando terminou com código %d", r.ExitCode)
}

// truncar corta a saída pelo FIM, guardando o começo.
//
// A mensagem que importa quase sempre está nas primeiras linhas — "não foi
// possível conectar", "permissão negada". O que vem depois costuma ser
// progresso. Guardar o fim daria a impressão de que o comando não disse nada.
func truncar(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	return s[:MaxOutputBytes] + "\n… (saída truncada)"
}

func asExitError(err error, alvo **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*alvo = e
		return true
	}
	return false
}

// GraceDepoisDoKill é quanto esperamos o `Wait` voltar depois de derrubar a
// árvore. SIGKILL não se ignora, então na prática volta na hora; o teto existe
// para o caso patológico de um neto que escapou do grupo (um daemon que fez
// duplo fork) e segue segurando o pipe. Preferimos devolver a saída incompleta
// a prender o agendamento para sempre.
const GraceDepoisDoKill = 5 * time.Second

// executarComPrazo roda o comando e, estourado o prazo, derruba a árvore.
func executarComPrazo(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	terminou := make(chan error, 1)
	go func() { terminou <- cmd.Wait() }()

	select {
	case err := <-terminou:
		return err
	case <-ctx.Done():
		_ = matarArvore(cmd)
		select {
		case err := <-terminou:
			return err
		case <-time.After(GraceDepoisDoKill):
			// A goroutine do `Wait` continua viva e ainda pode escrever no
			// buffer — por isso ele é protegido por mutex.
			return fmt.Errorf("comando não encerrou nem depois de derrubado")
		}
	}
}

// bufferSeguro é um buffer com trava.
//
// Necessário porque, no caso patológico acima, desistimos de esperar o `Wait` e
// lemos a saída enquanto a goroutine de cópia ainda pode estar escrevendo. Sem
// a trava isso é corrida de dados — e o `go test -race` da CI acusaria.
type bufferSeguro struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bufferSeguro) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bufferSeguro) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
