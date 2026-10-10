package bancada

import (
	"bufio"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Arvore descreve uma árvore sintética. A mesma descrição gera sempre os
// mesmos caminhos, tamanhos e conteúdos.
type Arvore struct {
	Raiz     string
	Arquivos int
	Semente  uint64
	// Mediana e Sigma da distribuição lognormal dos tamanhos, em bytes
	// (padrão: 4 KiB e 1,2). Teto corta a cauda (padrão: 1 MiB).
	Mediana float64
	Sigma   float64
	Teto    int64
}

func (a *Arvore) padroes() {
	if a.Mediana <= 0 {
		a.Mediana = 4096
	}
	if a.Sigma <= 0 {
		a.Sigma = 1.2
	}
	if a.Teto <= 0 {
		a.Teto = 1 << 20
	}
}

// nomes com acento, espaço e caixa mista entram de propósito: é onde as
// chaves costumam quebrar.
var nomesDePasta = []string{"dados", "config", "relatórios", "Fotos 2024", "src", "logs", "cache-app", "documentação", "backup", "tmp_x"}
var nomesDeArquivo = []string{"arquivo", "relatório", "ação", "nota fiscal", "IMG", "index", "leia-me", "dump"}
var extensoes = []string{".txt", ".dat", ".json", ".log", ".jpg", ".conf", ""}

// caminhoDe é o caminho relativo do arquivo i: profundidade de 1 a 6, com
// pastas de tamanhos desiguais.
func (a *Arvore) caminhoDe(i int) string {
	r := rand.New(rand.NewPCG(a.Semente, uint64(i)*2654435761+1))
	prof := 1 + r.IntN(6)
	p := ""
	// As pastas vêm de um espaço pequeno por nível: muitas se repetem, e a
	// árvore tem pastas com milhares de arquivos e pastas com um só.
	for n := 0; n < prof; n++ {
		p = filepath.Join(p, fmt.Sprintf("%s-%02d", nomesDePasta[r.IntN(len(nomesDePasta))], r.IntN(4+n*3)))
	}
	return filepath.Join(p, fmt.Sprintf("%s-%07d%s", nomesDeArquivo[r.IntN(len(nomesDeArquivo))], i, extensoes[r.IntN(len(extensoes))]))
}

func (a *Arvore) tamanhoDe(i int) int64 {
	r := rand.New(rand.NewPCG(a.Semente^0x9e3779b97f4a7c15, uint64(i)))
	t := int64(a.Mediana * math.Exp(r.NormFloat64()*a.Sigma))
	return min(max(t, 0), a.Teto)
}

// escrever grava n bytes pseudoaleatórios (mas fixos para a semente dada).
func escrever(caminho string, n int64, semente uint64) error {
	f, err := os.Create(caminho)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	r := rand.NewChaCha8([32]byte{byte(semente), byte(semente >> 8), byte(semente >> 16), byte(semente >> 24), byte(semente >> 32), byte(semente >> 40), byte(semente >> 48), byte(semente >> 56)})
	var buf [4096]byte
	for n > 0 {
		k := int64(len(buf))
		if n < k {
			k = n
		}
		_, _ = r.Read(buf[:k])
		if _, err := w.Write(buf[:k]); err != nil {
			f.Close()
			return err
		}
		n -= k
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// dataBase é a data de modificação do primeiro arquivo gerado; os seguintes
// vêm um segundo depois cada.
var dataBase = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// Gerar cria a árvore. Devolve o total de bytes.
func (a Arvore) Gerar() (int64, error) {
	a.padroes()
	var total int64
	feitas := map[string]bool{}
	for i := 0; i < a.Arquivos; i++ {
		p := filepath.Join(a.Raiz, a.caminhoDe(i))
		if d := filepath.Dir(p); !feitas[d] {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return total, err
			}
			feitas[d] = true
		}
		t := a.tamanhoDe(i)
		if err := escrever(p, t, a.Semente+uint64(i)); err != nil {
			return total, err
		}
		// Datas fixas e antigas, como numa árvore de verdade: arquivo
		// gravado há menos de 2 s é "data suspeita" para o agente, e o
		// backup seguinte o releria.
		quando := dataBase.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, quando, quando); err != nil {
			return total, err
		}
		total += t
	}
	return total, nil
}

// Listar devolve os arquivos da árvore, em ordem.
func Listar(raiz string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(raiz, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Mutacoes conhecidas por Mutar.
const (
	MutAlterar       = "alterar"         // acrescenta 32 bytes (tamanho e data mudam)
	MutAdicionar     = "adicionar"       // arquivos novos
	MutRemover       = "remover"         // apaga
	MutRenomearPasta = "renomear-pasta"  // renomeia uma pasta de primeiro nível
	MutTocar         = "tocar-sem-mudar" // muda só a data de modificação
	MutMudarSemTocar = "mudar-sem-tocar" // muda o conteúdo e repõe tamanho e data
)

// Mutar aplica uma mutação à árvore em raiz, escolhendo os arquivos com a
// semente dada. quanto é a fração dos arquivos (0–1) em alterar, remover,
// tocar-sem-mudar e mudar-sem-tocar; a quantidade em adicionar; ignorado em
// renomear-pasta. Devolve quantos arquivos foram afetados.
//
// mudar-sem-tocar repõe a data de modificação, mas não o ctime: o sistema não
// deixa. Onde o agente vê o ctime (Linux, macOS) a mudança é pega na hora; o
// caso "ninguém vê" é o do Windows, e nos testes é simulado sem o ctime.
func Mutar(raiz, mutacao string, quanto float64, semente uint64) (int, error) {
	r := rand.New(rand.NewPCG(semente, 0xa5a5))
	switch mutacao {
	case MutAdicionar:
		n := int(quanto)
		dir := filepath.Join(raiz, fmt.Sprintf("novos-%d", semente))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
		for i := 0; i < n; i++ {
			if err := escrever(filepath.Join(dir, fmt.Sprintf("novo-%07d.dat", i)), int64(512+r.IntN(4096)), semente<<20+uint64(i)); err != nil {
				return i, err
			}
		}
		return n, nil
	case MutRenomearPasta:
		entradas, err := os.ReadDir(raiz)
		if err != nil {
			return 0, err
		}
		var pastas []string
		for _, e := range entradas {
			if e.IsDir() {
				pastas = append(pastas, e.Name())
			}
		}
		if len(pastas) == 0 {
			return 0, fmt.Errorf("nenhuma pasta em %s", raiz)
		}
		p := pastas[r.IntN(len(pastas))]
		dentro, _ := Listar(filepath.Join(raiz, p))
		return len(dentro), os.Rename(filepath.Join(raiz, p), filepath.Join(raiz, p+"-renomeada"))
	}
	arquivos, err := Listar(raiz)
	if err != nil {
		return 0, err
	}
	n := int(math.Ceil(float64(len(arquivos)) * quanto))
	n = min(n, len(arquivos))
	escolhidos := r.Perm(len(arquivos))[:n]
	for _, i := range escolhidos {
		p := arquivos[i]
		switch mutacao {
		case MutAlterar:
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return 0, err
			}
			var extra [32]byte
			for j := range extra {
				extra[j] = byte(r.Uint32())
			}
			_, werr := f.Write(extra[:])
			if cerr := f.Close(); werr != nil || cerr != nil {
				return 0, fmt.Errorf("alterando %s: %v %v", p, werr, cerr)
			}
		case MutRemover:
			if err := os.Remove(p); err != nil {
				return 0, err
			}
		case MutTocar:
			agora := time.Now()
			if err := os.Chtimes(p, agora, agora); err != nil {
				return 0, err
			}
		case MutMudarSemTocar:
			if err := MudarSemTocar(p); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("mutação desconhecida: %s", mutacao)
		}
	}
	return n, nil
}

// MudarSemTocar inverte um byte do arquivo e repõe a data de modificação: o
// conteúdo muda com tamanho e data iguais. Arquivo vazio ganha… nada: não há
// como mudar sem mudar o tamanho, e ele fica como está.
func MudarSemTocar(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return nil
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	var b [1]byte
	pos := st.Size() / 2
	if _, err := f.ReadAt(b[:], pos); err != nil {
		f.Close()
		return err
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], pos); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(p, st.ModTime(), st.ModTime())
}
