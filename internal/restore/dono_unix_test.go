//go:build !windows

package restore

import (
	"os"
	"syscall"
	"testing"
)

func donoDe(t *testing.T, p string) (int, int) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sys().(*syscall.Stat_t)
	return int(s.Uid), int(s.Gid)
}
