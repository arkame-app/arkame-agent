// Package version expõe info de build injetada via -ldflags.
package version

// Set via -ldflags "-X github.com/arkame-app/agent/pkg/version.Version=..."
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String retorna "vVersion (commit Commit)".
func String() string {
	if Commit == "unknown" {
		return Version
	}
	return Version + " (commit " + Commit + ")"
}
