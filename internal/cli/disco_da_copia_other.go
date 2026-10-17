//go:build !linux

package cli

import "io"

// avisarDoDisco: só no Linux (os pontos de montagem vêm do /proc).
func avisarDoDisco(string, io.Writer) {}
