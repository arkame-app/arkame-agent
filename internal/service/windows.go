//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/arkame-app/agent/internal/config"
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

	// Reinstalar por cima: remove o serviço anterior de mesmo nome.
	if existing, err := m.OpenService(opts.Name); err == nil {
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
		StartCmd:  fmt.Sprintf("Restart-Service %s", opts.Name),
		StatusCmd: fmt.Sprintf("Get-Service %s", opts.Name),
		LogsCmd:   fmt.Sprintf(`Get-EventLog -LogName Application -Source %s -Newest 50`, opts.Name),
	}, nil
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

	// Parar antes de remover; se já estiver parado o SCM devolve erro, que aqui
	// não é problema.
	_, _ = s.Control(svc.Stop)

	if err := s.Delete(); err != nil {
		return fmt.Errorf("removendo o serviço %s: %w", opts.Name, err)
	}
	slog.Info("serviço removido", "name", opts.Name)
	return nil
}
