package main

import "syscall"

// rssEmMB: no macOS o ru_maxrss vem em bytes.
func rssEmMB(uso any) float64 {
	if ru, ok := uso.(*syscall.Rusage); ok {
		return float64(ru.Maxrss) / (1 << 20)
	}
	return 0
}
