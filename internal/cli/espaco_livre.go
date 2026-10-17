package cli

import "fmt"

func bytesParaLer(b uint64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.0f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
}
