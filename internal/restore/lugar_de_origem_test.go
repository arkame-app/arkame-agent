package restore

import "testing"

// noLugarDeOrigem decide, no Windows, se a pasta criada nasce com a DACL do
// segredo (pasta de restauração nova) ou herda a da mãe (lugar de origem).
func TestNoLugarDeOrigem(t *testing.T) {
	casos := []struct {
		nome       string
		windows    bool
		chaveFinal string
		sourceKey  string
		quer       bool
	}{
		{"arquivo no lugar", false, "home/ana/proj/relatorio.odt", "arkame/ag1/home/ana/proj/relatorio.odt", true},
		{"chave sem prefixo", false, "home/ana/proj/relatorio.odt", "home/ana/proj/relatorio.odt", true},
		{"pasta nova de restauração", false, "restore/home/ana/proj/relatorio.odt", "arkame/ag1/home/ana/proj/relatorio.odt", false},
		{"fim de nome não é fim de caminho", false, "proj/relatorio.odt", "arkame/ag1/home/ana/xproj/relatorio.odt", false},
		{"Windows no lugar", true, "C:/Users/Ana/Proj/relatorio.odt", "arkame/ag1/C:/Users/ana/proj/relatorio.odt", true},
		{"Windows em C:\\Restaurados", true, "C:/Restaurados/C/Users/ana/proj/relatorio.odt", "arkame/ag1/C:/Users/ana/proj/relatorio.odt", false},
		{"Linux diferencia maiúsculas", false, "home/Ana/relatorio.odt", "arkame/ag1/home/ana/relatorio.odt", false},
		{"chave vazia", false, "", "arkame/ag1/x", false},
	}
	for _, c := range casos {
		if got := noLugarDeOrigem(c.windows, c.chaveFinal, c.sourceKey); got != c.quer {
			t.Errorf("%s: noLugarDeOrigem = %v, queria %v", c.nome, got, c.quer)
		}
	}
}
