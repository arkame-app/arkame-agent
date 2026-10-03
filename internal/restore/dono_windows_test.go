//go:build windows

package restore

import "testing"

func donoDe(t *testing.T, _ string) (int, int) {
	t.Skip("sem dono numérico no Windows")
	return 0, 0
}
