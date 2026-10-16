package localstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Supervisor mantém o RustFS de pé enquanto o processo do agente vive.
type Supervisor struct {
	// Programa é o executável do RustFS.
	Programa string
	// Dados é a pasta onde o RustFS guarda os objetos.
	Dados string
	// Endereco é onde ele escuta: sempre 127.0.0.1:porta.
	Endereco string
	// ArquivoDaChave e ArquivoDoSegredo têm a credencial (fora da linha de
	// comando, que outro usuário da máquina leria na lista de processos).
	ArquivoDaChave   string
	ArquivoDoSegredo string
	// Log recebe a saída do RustFS (nil: descartada).
	Log string
}

// Esperas entre tentativas de subir de novo: dobra a cada queda rápida, até 1 minuto.
var (
	esperaMinima  = time.Second
	esperaMaxima  = time.Minute
	vidaQueZera   = time.Minute // de pé por mais que isto, a queda seguinte recomeça da espera mínima
	conferirOrfao = 30 * time.Second
)

const maxLog = 50 << 20

// URL de saúde do RustFS (200 quando está servindo).
func (s *Supervisor) urlDeSaude() string { return "http://" + s.Endereco + "/health" }

// Responde diz se há um RustFS servindo no endereço agora.
func (s *Supervisor) Responde(ctx context.Context) bool {
	c, cancelar := context.WithTimeout(ctx, 3*time.Second)
	defer cancelar()
	req, err := http.NewRequestWithContext(c, http.MethodGet, s.urlDeSaude(), nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// Pronto espera o RustFS responder, até o prazo.
func (s *Supervisor) Pronto(ctx context.Context, prazo time.Duration) error {
	limite := time.Now().Add(prazo)
	for {
		if s.Responde(ctx) {
			return nil
		}
		if time.Now().After(limite) {
			return fmt.Errorf("a cópia local (RustFS em %s) não respondeu em %s", s.Endereco, prazo)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Rodar mantém o RustFS de pé até o ctx acabar. Não devolve erro por queda:
// sobe de novo, com espera crescente, e registra.
//
// Um RustFS que já responde no endereço antes de subir (um filho que sobrou de
// um agente morto à força, que segura a porta) é adotado: o supervisor só
// confere, e sobe o seu quando ele parar de responder. Brigar pela porta
// deixaria a cópia local em laço de quedas.
func (s *Supervisor) Rodar(ctx context.Context) {
	espera := esperaMinima
	for ctx.Err() == nil {
		if s.Responde(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(conferirOrfao):
			}
			continue
		}
		inicio := time.Now()
		err := s.uma(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(inicio) > vidaQueZera {
			espera = esperaMinima
		}
		slog.Warn("a cópia local (RustFS) parou; subindo de novo", "err", err, "espera", espera)
		select {
		case <-ctx.Done():
			return
		case <-time.After(espera):
		}
		espera = min(espera*2, esperaMaxima)
	}
}

// uma roda o RustFS até ele sair ou o ctx acabar.
func (s *Supervisor) uma(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.Programa, "server",
		"--address", s.Endereco,
		"--access-key-file", s.ArquivoDaChave,
		"--secret-key-file", s.ArquivoDoSegredo,
		s.Dados,
	)
	cmd.Env = ambienteDoFilho()
	// Parar com calma (o RustFS fecha os arquivos) e, se não sair em 15 s, matar.
	cmd.Cancel = func() error { return interromper(cmd.Process) }
	cmd.WaitDelay = 15 * time.Second
	if s.Log != "" {
		f, err := abrirLog(s.Log)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	} else {
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("subindo o RustFS: %w", err)
	}
	slog.Info("cópia local (RustFS) no ar", "endereco", s.Endereco, "pid", cmd.Process.Pid)
	return cmd.Wait()
}

// ambienteDoFilho: o mínimo para o programa rodar, e nada das variáveis do
// agente — o env-file tem a chave do bucket, e o RustFS não precisa dela.
func ambienteDoFilho() []string {
	manter := map[string]bool{"PATH": true, "SYSTEMROOT": true, "WINDIR": true, "TEMP": true, "TMP": true, "TMPDIR": true, "HOME": true, "USERPROFILE": true, "PROGRAMDATA": true, "LANG": true}
	var env []string
	for _, kv := range os.Environ() {
		nome, _, _ := strings.Cut(kv, "=")
		if manter[strings.ToUpper(nome)] {
			env = append(env, kv)
		}
	}
	// O console web do RustFS fica desligado: a cópia local não é um serviço
	// de rede, e quem a opera é o agente.
	return append(env, "RUSTFS_CONSOLE_ENABLE=false")
}

// abrirLog abre o log para acrescentar; passou do teto, recomeça vazio.
func abrirLog(caminho string) (*os.File, error) {
	if fi, err := os.Stat(caminho); err == nil && fi.Size() > maxLog {
		_ = os.Truncate(caminho, 0)
	}
	return os.OpenFile(caminho, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
