//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// pastaFechada: a pasta é deste usuário (o root, no serviço), não é um link e
// ninguém mais grava nela — senão outro usuário da máquina poria ali uma
// "restauração" para o agente importar e apagar.
var pastaFechada = func(dir string) error {
	if err := donaEFechada(dir, true); err != nil {
		return err
	}
	// As pastas de cima também: quem grava numa delas troca a de baixo por um
	// link durante a importação, que é longa (revisão, 17/10 — a pasta deixou
	// de ser fixa em /var/backups, que é só do root).
	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		if err := donaEFechada(p, false); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// donaEFechada: a pasta é do root ou deste usuário e ninguém mais grava nela.
// A própria (exata) tem de ser deste usuário; as de cima podem ser do root.
func donaEFechada(dir string, exata bool) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("não é uma pasta")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("sem dono")
	}
	if int(st.Uid) != os.Geteuid() && (exata || st.Uid != 0) {
		return fmt.Errorf("é de outro usuário (uid %d)", st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("outros usuários gravam nela")
	}
	return nil
}
