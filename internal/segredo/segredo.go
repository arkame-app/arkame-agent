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
	if err := os.WriteFile(tmp, dados, 0o600); err != nil {
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
	return nil
}

// Proteger deixa um arquivo que já existe legível só pelo administrador (0600;
// no Windows, Administradores e SYSTEM).
func Proteger(caminho string) error { return protegerComModo(caminho, 0o600) }

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
