//go:build !linux && !darwin

package main

// rssEmMB: fora do Linux e do macOS a bancada compila, mas não mede memória.
func rssEmMB(any) float64 { return 0 }
