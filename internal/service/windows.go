//go:build windows

package service

import (
	"context"
	"fmt"

	"github.com/arkame-app/agent/internal/config"
)

// installPlatform no Windows registra o binário como Windows Service
// via sc.exe ou golang.org/x/sys/windows/svc/mgr. TODO: stub por ora.
func installPlatform(_ context.Context, _ *config.Config) error {
	return fmt.Errorf("service install on Windows: ainda não implementado (TODO: Windows Service via x/sys/windows/svc)")
}
