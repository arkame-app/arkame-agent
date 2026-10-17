package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// sistemasDeArquivosDeDados: os que guardam dados num disco (não tmpfs,
// overlay, rede nem pseudo).
var sistemasDeArquivosDeDados = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "jfs": true, "bcachefs": true}

// avisarDoDisco mostra o espaço livre do disco da pasta da cópia local e
// avisa quando há outro disco com muito mais espaço. A cópia local guarda o
// mesmo volume do backup, mais as versões: no Zimbra Adubos (17/10) ela foi
// para /srv, no / de 49 GB, com 846 GB livres no /opt.
func avisarDoDisco(pasta string, out io.Writer) {
	livre, dev, ok := espacoEDispositivo(pasta)
	if !ok {
		return
	}
	fmt.Fprintf(out, "  ✓ Espaço livre no disco de %s: %s\n", pasta, bytesParaLer(livre))
	melhor, livreMelhor := "", uint64(0)
	for _, ponto := range pontosDeMontagem() {
		l, d, ok := espacoEDispositivo(ponto)
		if ok && d != dev && l > livreMelhor {
			melhor, livreMelhor = ponto, l
		}
	}
	if melhor != "" && livreMelhor >= 2*livre && livreMelhor-livre >= 50<<30 {
		fmt.Fprintf(out, "  ! %s tem %s livres, bem mais que o disco desta pasta. A cópia local guarda o mesmo volume do backup, com as versões:\n"+
			"    prefira uma pasta lá (no painel, troque a pasta para %s e rode o comando de novo).\n",
			melhor, bytesParaLer(livreMelhor), strings.TrimRight(melhor, "/")+"/arkame-local")
	}
}

func espacoEDispositivo(p string) (livre, dev uint64, ok bool) {
	var st syscall.Statfs_t
	var info syscall.Stat_t
	if syscall.Statfs(p, &st) != nil || syscall.Stat(p, &info) != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(info.Dev), true
}

func pontosDeMontagem() []string {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var r []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		c := strings.Fields(s.Text())
		if len(c) < 4 || !sistemasDeArquivosDeDados[c[2]] {
			continue
		}
		ponto := strings.ReplaceAll(c[1], `\040`, " ")
		if ponto == "/boot" || strings.HasPrefix(ponto, "/boot/") || strings.HasPrefix(c[3], "ro") {
			continue
		}
		r = append(r, ponto)
	}
	return r
}
