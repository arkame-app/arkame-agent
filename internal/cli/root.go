package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

var (
	verbose  bool
	logJSON  bool
	logLevel string
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "arkame-agent",
		Short:   "Arkame — agent de backup para BYOS",
		Long:    "Arkame é um SaaS de backup em nuvem com BYOS. Este agent lê arquivos do servidor onde roda e envia ao bucket do cliente, reportando status ao painel.",
		Version: version.String(),
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			setupLogger()
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "log detalhado (debug)")
	root.PersistentFlags().BoolVar(&logJSON, "log-json", false, "formato JSON para logs (produção)")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "nível de log: debug | info | warn | error")

	root.AddCommand(
		newInstallCmd(),
		newRunCmd(),
		newServiceCmd(),
		newStatusCmd(),
		newCheckStorageCmd(),
		newSetStorageKeysCmd(),
		newUninstallCmd(),
		newSetupCmd(),
		newHeartbeatCmd(),
		newVersionCmd(),
	)
	return root
}

// Execute roda o root command. Chamado por main.
func Execute() error {
	return newRootCmd().Execute()
}

func setupLogger() {
	lvl := slog.LevelInfo
	switch logLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	if verbose {
		lvl = slog.LevelDebug
	}

	logNivel = lvl
	logPara(os.Stderr)
}

var logNivel = slog.LevelInfo

// logPara troca o destino do log, com o nível e o formato das flags.
func logPara(w io.Writer) {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: logNivel}
	if logJSON {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	slog.SetDefault(slog.New(handler))
}
