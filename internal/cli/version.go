package cli

import (
	"fmt"

	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Imprime build info",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("arkame-agent", version.String())
			fmt.Println("commit:    ", version.Commit)
			fmt.Println("built:     ", version.BuildDate)
		},
	}
}
