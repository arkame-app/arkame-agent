package sync

import (
	"io/fs"
	"syscall"
)

func ctimeDe(info fs.FileInfo) (int64, uint32) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ctimespec.Sec), uint32(st.Ctimespec.Nsec)
	}
	return 0, 0
}
