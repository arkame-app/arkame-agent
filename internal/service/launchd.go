//go:build darwin

package service

import (
	"context"
	"fmt"

	"github.com/arkame-app/agent/internal/config"
)

// installPlatform no macOS cria /Library/LaunchDaemons/app.arkame.agent.plist
// e roda launchctl load -w. TODO: implementação completa — por ora stub.
func installPlatform(_ context.Context, _ *config.Config) error {
	return fmt.Errorf("service install on macOS: ainda não implementado (TODO: launchd plist)")
}
