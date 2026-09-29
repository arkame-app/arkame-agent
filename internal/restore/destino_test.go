package restore

import "testing"

// A unidade no começo do caminho (chave de servidor Windows) vira pasta.
func TestSplitDestFilenameUnidade(t *testing.T) {
	sub, base, err := splitDestFilename("C:/Users/Greyce/a.txt")
	if err != nil || sub != "C/Users/Greyce" || base != "a.txt" {
		t.Fatalf("sub=%q base=%q err=%v", sub, base, err)
	}
	if _, _, err := splitDestFilename("../a.txt"); err == nil {
		t.Fatal(".. continua recusado")
	}
}
