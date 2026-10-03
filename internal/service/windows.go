//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// No Windows não existe equivalente prático ao systemd --user para um serviço
// que precisa sobreviver ao logout: todo serviço é registrado no SCM, que exige
// privilégio de administrador.
func defaultScope() Scope { return ScopeSystem }

func installPlatform(_ context.Context, cfg *config.Config, opts Options) (*Installed, error) {
	if opts.Scope == ScopeUser {
		return nil, fmt.Errorf(
			"o Windows não tem serviço por usuário: rode este comando num PowerShell como Administrador (sem --service-scope=user)")
	}

	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf(
			"conectando ao gerenciador de serviços: %w (abra o PowerShell como Administrador)", err)
	}
	defer m.Disconnect()

	// Reinstalar por cima: para e remove o serviço anterior de mesmo nome.
	// Sem parar, o Windows só o marca para remoção, e criar o novo com o mesmo
	// nome falha até ele sair.
	if existing, err := m.OpenService(opts.Name); err == nil {
		pararEEsperar(existing)
		_ = existing.Delete()
		existing.Close()
		slog.Info("serviço anterior removido para reinstalar", "name", opts.Name)
	}

	cfgWin := mgr.Config{
		DisplayName:  "Arkame Backup Agent (" + opts.Name + ")",
		Description:  "Faz backup deste servidor para o seu próprio bucket. https://arkame.app/docs",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}

	s, err := m.CreateService(opts.Name, opts.BinaryPath, cfgWin, "run", "--config", cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("criando o serviço %s: %w", opts.Name, err)
	}
	defer s.Close()

	// Reinício automático em caso de falha: 10s, 10s, depois a cada 60s.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10_000_000_000},
		{Type: mgr.ServiceRestart, Delay: 10_000_000_000},
		{Type: mgr.ServiceRestart, Delay: 60_000_000_000},
	}, 86400); err != nil {
		slog.Warn("não consegui configurar reinício automático do serviço", "err", err)
	}

	if opts.Start {
		if err := s.Start(); err != nil {
			return nil, fmt.Errorf("iniciando o serviço %s: %w", opts.Name, err)
		}
	}

	return &Installed{
		Name:      opts.Name,
		Scope:     ScopeSystem,
		UnitPath:  strings.Join([]string{`HKLM\SYSTEM\CurrentControlSet\Services`, opts.Name}, `\`),
		StartCmd:  RestartCommand(opts.Name, ScopeSystem),
		StatusCmd: fmt.Sprintf("Get-Service %s", opts.Name),
		LogsCmd:   fmt.Sprintf(`Get-Content -Tail 50 -Wait '%s'`, ArquivoDeLog(cfg.ConfigPath)),
	}, nil
}

// servicosDoAgente: os serviços do agente registrados no SCM — os
// arkame-agent* e os que chamam o programa exe com outro nome.
func servicosDoAgente(exe string) []string {
	m, err := mgr.Connect()
	if err != nil {
		return nil
	}
	defer m.Disconnect()
	todos, err := m.ListServices()
	if err != nil {
		return nil
	}
	var nomes []string
	for _, n := range todos {
		if strings.HasPrefix(strings.ToLower(n), DefaultName) || servicoChamaPrograma(m, n, exe) {
			nomes = append(nomes, n)
		}
	}
	return nomes
}

// servicoChamaPrograma diz se o BinaryPathName do serviço roda o programa
// exe. Abre só com SERVICE_QUERY_CONFIG: o mgr.OpenService pede acesso total,
// que serviços do sistema negam.
func servicoChamaPrograma(m *mgr.Mgr, nome, exe string) bool {
	if exe == "" {
		return false
	}
	p, err := windows.UTF16PtrFromString(nome)
	if err != nil {
		return false
	}
	h, err := windows.OpenService(m.Handle, p, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return false
	}
	s := &mgr.Service{Name: nome, Handle: h}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return false
	}
	return mesmoPrograma("windows", programaDaLinha(c.BinaryPathName), exe)
}

// configDoServico lê o --config da linha de comando registrada no SCM. O SCM
// só tem serviços da máquina: o escopo não muda nada.
func configDoServico(nome string, _ Scope) (string, bool) {
	m, err := mgr.Connect()
	if err != nil {
		return "", false
	}
	defer m.Disconnect()
	s, err := m.OpenService(nome)
	if err != nil {
		return "", false
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return "", false
	}
	args, err := windows.DecomposeCommandLine(c.BinaryPathName)
	if err != nil {
		return "", false
	}
	cfg := configDosArgs(args)
	return cfg, cfg != ""
}

// pararEEsperar para o serviço e espera ele parar de fato: o processo pode
// estar no meio de um backup, e enquanto ele vive o programa fica travado no
// disco. Já parado, o SCM devolve erro no Control, que aqui não é problema.
func pararEEsperar(s *mgr.Service) {
	_, _ = s.Control(svc.Stop)
	for i := 0; i < 60; i++ {
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(time.Second)
	}
}

// Parar para o serviço, se existir — para trocar o programa por cima.
func Parar(name string) {
	m, err := mgr.Connect()
	if err != nil {
		return
	}
	defer m.Disconnect()
	if s, err := m.OpenService(name); err == nil {
		pararEEsperar(s)
		s.Close()
	}
}

func restartArgs(name string, _ Scope) []string {
	return []string{"powershell", "-NoProfile", "-Command", "Restart-Service " + name}
}

func uninstallPlatform(_ context.Context, opts Options) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectando ao gerenciador de serviços: %w (abra o PowerShell como Administrador)", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(opts.Name)
	if err != nil {
		// Serviço já não existe: nada a fazer.
		return nil
	}
	defer s.Close()

	pararEEsperar(s)

	if err := s.Delete(); err != nil {
		return fmt.Errorf("removendo o serviço %s: %w", opts.Name, err)
	}
	slog.Info("serviço removido", "name", opts.Name)
	return nil
}
