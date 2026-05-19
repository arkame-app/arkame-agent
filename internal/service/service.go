// Package service instala o agent como serviço do SO
// (systemd no Linux, launchd no macOS, Windows Service no Windows).
//
// ATENÇÃO: stubs por ora — cada OS tem seu arquivo build-tagged
// (systemd.go, launchd.go, windows.go) para manter o binário cross-platform.
package service

import (
	"context"

	"github.com/arkame-app/agent/internal/config"
)

// Install orquestra a instalação como serviço. Delega para o platform-specific.
func Install(ctx context.Context, cfg *config.Config) error {
	return installPlatform(ctx, cfg)
}
