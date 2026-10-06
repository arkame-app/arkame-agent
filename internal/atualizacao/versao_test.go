package atualizacao

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLerVersao(t *testing.T) {
	for texto, quer := range map[string]Versao{
		"0.4.31":   {0, 4, 31},
		"v0.4.31":  {0, 4, 31},
		" 1.0.0\n": {1, 0, 0},
		"10.20.30": {10, 20, 30},
	} {
		if v, err := LerVersao(texto); err != nil || v != quer {
			t.Errorf("LerVersao(%q) = %v, %v", texto, v, err)
		}
	}
	for _, texto := range []string{"", "dev", "0.4", "0.4.31.1", "0.4.x", "v0.4.31-3-gabc", "0.4.31-rc1", "0.+4.1", "0.-4.1", "1..2", "vv1.2.3"} {
		if v, err := LerVersao(texto); err == nil {
			t.Errorf("LerVersao(%q) aceitou: %v", texto, v)
		}
	}
}

// A ordem é numérica, campo a campo: 0.4.31 < 0.4.100 < 0.10.0 < 1.0.0.
func TestOrdemDasVersoes(t *testing.T) {
	ordem := []string{"0.4.9", "0.4.31", "0.4.100", "0.5.0", "0.10.0", "1.0.0"}
	for i, a := range ordem {
		va, _ := LerVersao(a)
		for j, b := range ordem {
			vb, _ := LerVersao(b)
			if got := va.Maior(vb); got != (i > j) {
				t.Errorf("%s maior que %s = %v", a, b, got)
			}
		}
	}
}

func TestAssinarEVerificar(t *testing.T) {
	pub, priv := chavesDeTesteGeradas(t)
	v, _ := LerVersao("0.5.0")
	checksums := []byte("abc  arquivo\n")
	sig := Assinar(priv, v, checksums)
	if err := VerificarAssinatura([]ed25519.PublicKey{pub}, v, checksums, sig); err != nil {
		t.Fatalf("a assinatura certa não conferiu: %v", err)
	}
	outra, _ := LerVersao("0.5.1")
	for nome, err := range map[string]error{
		"outra versão":   VerificarAssinatura([]ed25519.PublicKey{pub}, outra, checksums, sig),
		"outro conteúdo": VerificarAssinatura([]ed25519.PublicKey{pub}, v, []byte("abd  arquivo\n"), sig),
		"sem chaves":     VerificarAssinatura(nil, v, checksums, sig),
		"vazia":          VerificarAssinatura([]ed25519.PublicKey{pub}, v, checksums, nil),
		"curta":          VerificarAssinatura([]ed25519.PublicKey{pub}, v, checksums, []byte("YWJj")),
	} {
		if err == nil {
			t.Errorf("%s: a assinatura conferiu", nome)
		}
	}
	// A assinatura é só do conteúdo com o prefixo: a de um arquivo qualquer,
	// feita com a mesma chave para outro fim, não serve.
	crua := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, checksums))
	if VerificarAssinatura([]ed25519.PublicKey{pub}, v, checksums, []byte(crua)) == nil {
		t.Error("a assinatura sem o prefixo e a versão conferiu")
	}
}

func TestResumoNoChecksums(t *testing.T) {
	sha := strings.Repeat("0a", 32)
	texto := []byte(sha + "  arkame-agent_linux_amd64\n" + strings.ToUpper(sha) + " *arkame-agent_windows_amd64.exe\nlixo\n" + "abc  curto\n")
	for _, nome := range []string{"arkame-agent_linux_amd64", "arkame-agent_windows_amd64.exe"} {
		if got, err := ResumoNoChecksums(texto, nome); err != nil || got != sha {
			t.Errorf("%s: %q, %v", nome, got, err)
		}
	}
	for _, nome := range []string{"arkame-agent_linux", "curto", "ausente", ""} {
		if got, err := ResumoNoChecksums(texto, nome); err == nil {
			t.Errorf("%s: devolveu %q", nome, got)
		}
	}
}

func TestLugarDe(t *testing.T) {
	l := LugarDe(filepath.Join("/usr/local/bin", "arkame-agent"))
	if l.Novo != "/usr/local/bin/arkame-agent.novo" || l.Anterior != "/usr/local/bin/arkame-agent.anterior" ||
		l.Estado != "/usr/local/bin/arkame-agent.atualizacao.json" {
		t.Fatalf("lugar = %+v", l)
	}
	// No Windows a extensão fica no fim: o programa novo é executado.
	w := LugarDe(filepath.Join("C:", "Arkame", "arkame-agent.exe"))
	if !strings.HasSuffix(w.Novo, "arkame-agent.novo.exe") || !strings.HasSuffix(w.Anterior, "arkame-agent.anterior.exe") ||
		!strings.HasSuffix(w.Estado, "arkame-agent.atualizacao.json") {
		t.Fatalf("lugar no Windows = %+v", w)
	}
}

// O impedimento de cada instalação que não pode se atualizar sozinha — e
// nenhum para a que pode.
func TestImpedimento(t *testing.T) {
	dir := t.TempDir()
	boa := Instalacao{Programa: filepath.Join(dir, "arkame-agent"), VersaoAtual: "0.5.0", SobServico: true, TemChaves: true}
	if got := boa.Impedimento(); got != "" {
		t.Fatalf("instalação boa impedida: %q", got)
	}
	casos := map[string]func(*Instalacao){
		ImpedidaDesligada:   func(i *Instalacao) { i.Desligada = true },
		ImpedidaVersaoDeDev: func(i *Instalacao) { i.VersaoAtual = "dev" },
		ImpedidaSemChaves:   func(i *Instalacao) { i.TemChaves = false },
		ImpedidaSemServico:  func(i *Instalacao) { i.SobServico = false },
	}
	for quer, estragar := range casos {
		i := boa
		estragar(&i)
		if got := i.Impedimento(); got != quer {
			t.Errorf("impedimento = %q, queria %q", got, quer)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("como root, toda pasta aceita escrita")
	}
	soLeitura := filepath.Join(dir, "so-leitura")
	if err := os.Mkdir(soLeitura, 0o555); err != nil {
		t.Fatal(err)
	}
	i := boa
	i.Programa = filepath.Join(soLeitura, "arkame-agent")
	if got := i.Impedimento(); got != ImpedidaPastaSoLeitura {
		t.Errorf("pasta só de leitura: impedimento = %q", got)
	}
}
