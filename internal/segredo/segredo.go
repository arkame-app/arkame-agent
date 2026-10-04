// Package segredo grava os arquivos sensíveis do agente — chave do bucket,
// token, chave privada, identidade — legíveis só pelo administrador.
//
// No Linux e no macOS isso é o modo do arquivo (0600). No Windows o modo não
// restringe nada: o os.WriteFile(…, 0600) deixava o token e a chave privada
// com a ACL herdada de C:\etc, legíveis por qualquer usuário da máquina. Lá a
// proteção é uma DACL só com Administradores e SYSTEM (ver proteger_windows.go),
// aplicada no arquivo e na pasta que o agente cria.
package segredo

import (
	"fmt"
	"os"
	"path/filepath"
)

// Gravar escreve o arquivo protegido, por troca atômica: grava ao lado, protege
// e só então põe no lugar — o conteúdo nunca existe no disco sem a proteção, e
// um arquivo antigo com permissão larga (0644) é substituído, não reaproveitado
// (o os.WriteFile mantém a permissão de um arquivo que já existe).
//
// modo vale fora do Windows (0600 para segredo; 0644 para o que só precisa de
// integridade, como o agent.id). No Windows é sempre Administradores e SYSTEM.
func Gravar(caminho string, dados []byte, modo os.FileMode) error {
	if err := CriarPasta(filepath.Dir(caminho)); err != nil {
		return err
	}
	tmp := caminho + ".novo"
	if err := escreverSincronizado(tmp, dados); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	if err := protegerComModo(tmp, modo); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, caminho); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	SincronizarPasta(filepath.Dir(caminho))
	return nil
}

// escreverSincronizado grava o temporário e o leva ao disco (fsync) antes de
// fechar. Sem o fsync, o rename podia chegar ao disco antes dos dados (XFS, e
// ext4/btrfs conforme a montagem): depois de um corte de energia, o token
// renovado voltava com 0 bytes e o agente não se autenticava mais.
func escreverSincronizado(tmp string, dados []byte) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(dados); err != nil {
		f.Close()
		return err
	}
	if err := sincronizarArquivo(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sincronizarArquivo e sincronizarPasta são variáveis para o teste conferir a
// ordem: o arquivo vai ao disco antes do rename, a pasta depois.
var (
	sincronizarArquivo = (*os.File).Sync
	sincronizarPasta   = sincronizarPastaSO
)

// SincronizarPasta leva ao disco a entrada da pasta depois de um rename, para
// a troca sobreviver a um corte de energia. Fora do Windows abre a pasta e
// chama fsync; no Windows não faz nada. É o melhor possível: o rename já foi
// feito e, se a pasta não sincronizar (há sistemas de arquivos que recusam
// fsync em pasta), o pior caso é voltar o arquivo anterior, inteiro.
func SincronizarPasta(dir string) { _ = sincronizarPasta(dir) }

// Proteger deixa um arquivo que já existe legível só pelo administrador (0600;
// no Windows, Administradores e SYSTEM).
func Proteger(caminho string) error { return protegerComModo(caminho, 0o600) }

// ProtegerPasta deixa uma pasta que o agente acabou de criar só para o
// administrador, herdado pelo que for gravado nela: no Windows, a DACL
// protegida de Administradores e SYSTEM; fora dele não faz nada (quem cria
// a pasta já lhe dá o modo).
func ProtegerPasta(dir string) error { return protegerPasta(dir) }

// CriarPasta cria a pasta da configuração. Se fomos nós que a criamos, ela já
// nasce protegida (no Windows, Administradores e SYSTEM, herdado pelo que for
// gravado nela); pasta que já existia não é tocada — pode ser uma pasta da
// pessoa, apontada por --config.
func CriarPasta(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criando %s: %w", dir, err)
	}
	return protegerPasta(dir)
}
