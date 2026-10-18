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
	// Endereco é onde ele responde a este agente: sempre 127.0.0.1:porta.
	Endereco string
	// Escuta, se dado, é onde o RustFS se prende (0.0.0.0:porta, para receber
	// backups da rede; `local-storage share`). A saúde continua conferida pelo
	// Endereco. Vazio: só no Endereco.
	Escuta string
	// ArquivoDaChave e ArquivoDoSegredo têm a credencial (fora da linha de
	// comando, que outro usuário da máquina leria na lista de processos).
	ArquivoDaChave   string
	ArquivoDoSegredo string
	// Log recebe a saída do RustFS (nil: descartada).
	Log string
	// PastaTLS tem o certificado e a chave do RustFS (PrepararTLS). Sem ela o
	// supervisor não sobe nada: em texto puro, quem escutasse na porta antes
	// do RustFS receberia o backup.
	PastaTLS string
	// SHA256 do programa, conferido antes de cada início: ele roda como
	// root/SYSTEM, e um programa trocado viraria um administrador da máquina.
	SHA256 string
	// Conferir, se dado, recusa o programa que quem não é administrador pode
	// trocar (o dono e a permissão do arquivo e das pastas acima).
	Conferir func(programa string) error
}

// Esperas entre tentativas de subir de novo: dobra a cada queda rápida, até 1 minuto.
var (
	esperaMinima  = time.Second
	esperaMaxima  = time.Minute
	vidaQueZera   = time.Minute // de pé por mais que isto, a queda seguinte recomeça da espera mínima
	conferirOrfao = 30 * time.Second
)

const maxLog = 50 << 20

// enderecoDeEscuta: onde o RustFS se prende.
func (s *Supervisor) enderecoDeEscuta() string {
	if s.Escuta != "" {
		return s.Escuta
	}
	return s.Endereco
}

// URL de saúde do RustFS (200 quando está servindo).
func (s *Supervisor) urlDeSaude() string { return "https://" + s.Endereco + "/health" }

// Responde diz se o NOSSO RustFS está servindo no endereço agora: só conta a
// resposta de quem apresenta o certificado da pasta TLS. Outro programa na
// porta (de outro usuário da máquina, ou de outro serviço) não é adotado.
func (s *Supervisor) Responde(ctx context.Context) bool {
	if s.PastaTLS == "" {
		return false
	}
	cli, err := clienteFixado(s.PastaTLS)
	if err != nil {
		return false
	}
	c, cancelar := context.WithTimeout(ctx, 3*time.Second)
	defer cancelar()
	req, err := http.NewRequestWithContext(c, http.MethodGet, s.urlDeSaude(), nil)
	if err != nil {
		return false
	}
	res, err := cli.Do(req)
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
// deixaria a cópia local em laço de quedas. Só é adotado quem apresenta o
// certificado da cópia local (Responde): a chave dele é só do administrador.
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
	if err := s.conferirPrograma(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.Programa, "server",
		"--address", s.enderecoDeEscuta(),
		"--access-key-file", s.ArquivoDaChave,
		"--secret-key-file", s.ArquivoDoSegredo,
		"--tls-path", s.PastaTLS,
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
	// No Windows o filho vai num Job Object que morre com o agente: sem isso,
	// um serviço encerrado à força deixava o rustfs.exe rodando sozinho.
	if solte, err := prenderAoAgente(cmd.Process); err != nil {
		slog.Warn("a cópia local (RustFS) não ficou presa ao agente; se o serviço for morto à força ela continua de pé", "err", err)
	} else {
		defer solte()
	}
	slog.Info("cópia local (RustFS) no ar", "endereco", s.enderecoDeEscuta(), "pid", cmd.Process.Pid)
	return cmd.Wait()
}

// conferirPrograma: antes de cada início, o programa tem de ser o que o
// install deixou (SHA-256) e só o administrador pode trocá-lo.
func (s *Supervisor) conferirPrograma() error {
	if s.PastaTLS == "" {
		return fmt.Errorf("a cópia local está sem a pasta TLS (ARKAME_LOCAL_STORE_TLS): rode de novo o local-storage install")
	}
	if s.SHA256 == "" {
		return fmt.Errorf("a cópia local está sem o SHA-256 do programa (ARKAME_LOCAL_STORE_PROGRAM_SHA256): rode de novo o local-storage install")
	}
	if s.Conferir != nil {
		if err := s.Conferir(s.Programa); err != nil {
			return fmt.Errorf("o programa da cópia local foi recusado: %w", err)
		}
	}
	got, err := HashDoArquivo(s.Programa)
	if err != nil {
		return err
	}
	if got != s.SHA256 {
		return fmt.Errorf("o programa da cópia local (%s) mudou desde a instalação (SHA-256 %s, esperado %s): recusado; rode de novo o local-storage install", s.Programa, got, s.SHA256)
	}
	return nil
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

// abrirLog abre o log para acrescentar; passou do teto, recomeça vazio. Sem
// seguir link simbólico: o agente roda como root, e um link plantado no lugar
// do log o faria truncar ou escrever num arquivo do sistema.
func abrirLog(caminho string) (*os.File, error) {
	f, err := os.OpenFile(caminho, os.O_CREATE|os.O_WRONLY|os.O_APPEND|semSeguirLink, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("o log da cópia local (%s) não é um arquivo comum", caminho)
	}
	if fi.Size() > maxLog {
		_ = f.Truncate(0)
	}
	return f, nil
}
