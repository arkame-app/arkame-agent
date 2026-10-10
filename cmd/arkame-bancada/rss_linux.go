package main

import "syscall"

// rssEmMB: pico de memória residente do processo filho. No Linux o ru_maxrss
// vem em KiB.
func rssEmMB(uso any) float64 {
	if ru, ok := uso.(*syscall.Rusage); ok {
		return float64(ru.Maxrss) / 1024
	}
	return 0
}
