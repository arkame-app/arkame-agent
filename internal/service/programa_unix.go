//go:build !windows

package service

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// donoDoCaminho é o que a conferência do programa precisa de cada caminho:
// dono, grupo e permissões, sem seguir link.
type donoDoCaminho struct {
	uid, gid uint32
	modo     fs.FileMode
}

// lerDono e resolverLinks: variáveis para os testes montarem a árvore.
var (
	lerDono = func(p string) (donoDoCaminho, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return donoDoCaminho{}, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return donoDoCaminho{}, fmt.Errorf("sem dono conhecido para %s", p)
		}
		return donoDoCaminho{uid: st.Uid, gid: st.Gid, modo: fi.Mode()}, nil
	}
	resolverLinks = filepath.EvalSymlinks
)

// conferirPrograma recusa, para o serviço do sistema (que roda como root), o
// programa que outro usuário pode trocar. Sem isso, `sudo ~/.local/bin/
// arkame-agent install` deixava uma unit root com ExecStart no home do
// usuário: qualquer processo dele trocava o arquivo e virava root no próximo
// reinício do serviço (Restart=always; KeepAlive no launchd).
//
// Confere o programa e cada pasta acima dele, até a raiz — quem grava numa
// pasta do caminho renomeia o que está embaixo —, no caminho registrado e no
// caminho real (links resolvidos). Cada um precisa ser do root e não ser
// gravável por outros nem pelo grupo, salvo o grupo do root (gid 0). Link
// simbólico não tem permissão própria: vale o dono e a pasta onde está.
func conferirPrograma(programa string) error {
	abs, err := filepath.Abs(programa)
	if err != nil {
		return fmt.Errorf("conferindo o programa %s: %w", programa, err)
	}
	caminhos := []string{abs}
	if real, err := resolverLinks(abs); err == nil && real != abs {
		caminhos = append(caminhos, real)
	}
	for _, c := range caminhos {
		for p := c; ; p = filepath.Dir(p) {
			if problema := problemaDoCaminho(p); problema != "" {
				return fmt.Errorf(
					"o serviço do sistema roda como root e chamaria %s, mas %s: quem pode trocar esse arquivo viraria root no próximo reinício do serviço. "+
						"Instale o programa numa pasta só do root — o instalador com sudo põe em /usr/local/bin "+
						"(curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=SEU_CODIGO) —, "+
						"ou instale só para o seu usuário, sem sudo (escopo user): %s install --token=SEU_CODIGO",
					abs, problema, abs)
			}
			if d := filepath.Dir(p); d == p {
				break
			}
		}
	}
	return nil
}

// problemaDoCaminho descreve por que p não é só do root; vazio se é.
func problemaDoCaminho(p string) string {
	d, err := lerDono(p)
	if err != nil {
		return fmt.Sprintf("não consegui conferir o dono de %s (%v)", p, err)
	}
	if d.uid != 0 {
		return fmt.Sprintf("%s pertence a %s, e não ao root", p, nomeDoUsuario(d.uid))
	}
	if d.modo&fs.ModeSymlink != 0 {
		return ""
	}
	if d.modo.Perm()&0o002 != 0 {
		return fmt.Sprintf("%s é gravável por qualquer usuário", p)
	}
	if d.modo.Perm()&0o020 != 0 && d.gid != 0 {
		return fmt.Sprintf("%s é gravável pelo grupo %s", p, nomeDoGrupo(d.gid))
	}
	return ""
}

func nomeDoUsuario(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}
	return "o usuário " + id
}

func nomeDoGrupo(gid uint32) string {
	id := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(id); err == nil {
		return g.Name
	}
	return id
}
