package restore

import "testing"

func TestArquivoNaPastaWindows(t *testing.T) {
	casos := []struct {
		real, pasta, nome string
		quer              bool
	}{
		{`\\?\C:\Restaurados\.arkame-restore-1.txt`, `C:\Restaurados`, `.arkame-restore-1.txt`, true},
		{`\\?\C:\restaurados\A.TXT`, `\\?\C:\Restaurados\`, `a.txt`, true}, // maiúsculas e barra final
		{`\\?\C:\a.txt`, `C:\`, `a.txt`, true},                             // raiz da unidade
		{`\\?\UNC\srv\share\d\a.txt`, `\\srv\share\d`, `a.txt`, true},
		// Junção trocada no caminho: o arquivo foi parar em System32.
		{`\\?\C:\Windows\System32\a.txt`, `C:\Users\ana\docs`, `a.txt`, false},
		// Mesma pasta, outro nome; e subpasta da esperada.
		{`\\?\C:\Restaurados\b.txt`, `C:\Restaurados`, `a.txt`, false},
		{`\\?\C:\Restaurados\sub\a.txt`, `C:\Restaurados`, `a.txt`, false},
		// Prefixo de nome não é pasta: C:\Restaurados2 não está em C:\Restaurados.
		{`\\?\C:\Restaurados2\a.txt`, `C:\Restaurados`, `a.txt`, false},
		{`\\?\C:\Restaurados\a.txt`, `C:\Restaurados`, ``, false},
		{`\\?\C:\Restaurados\sub\a.txt`, `C:\Restaurados`, `sub\a.txt`, false},
	}
	for _, c := range casos {
		if got := arquivoNaPastaWindows(c.real, c.pasta, c.nome); got != c.quer {
			t.Errorf("arquivoNaPastaWindows(%q, %q, %q) = %v, queria %v", c.real, c.pasta, c.nome, got, c.quer)
		}
	}
}

func TestMesmoCaminhoWindows(t *testing.T) {
	if !mesmoCaminhoWindows(`\\?\C:\Restaurados`, `c:\restaurados\`) {
		t.Error("mesma pasta, com prefixo e maiúsculas diferentes")
	}
	if mesmoCaminhoWindows(`\\?\D:\Restaurados`, `C:\Restaurados`) {
		t.Error("unidades diferentes")
	}
	if mesmoCaminhoWindows(`\\?\C:\Windows\System32`, `C:\Users\ana\docs`) {
		t.Error("pasta desviada")
	}
}
