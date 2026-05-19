//go:build linux

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/arkame-app/agent/internal/config"
)

const systemdUnit = `[Unit]
Description=Arkame Backup Agent
Documentation=https://docs.arkame.app
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/arkame-agent run --config %s
Restart=always
RestartSec=5s
User=root
Group=root
EnvironmentFile=-%s

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/etc/arkame /var/lib/arkame
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`

func installPlatform(_ context.Context, cfg *config.Config) error {
	unit := fmt.Sprintf(systemdUnit, cfg.PrivateKeyPath, cfg.PrivateKeyPath)
	path := "/etc/systemd/system/arkame-agent.service"

	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("escrevendo %s: %w", path, err)
	}
	slog.Info("systemd unit criado", "path", path)

	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "enable", "--now", "arkame-agent").Run(); err != nil {
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	return nil
}
