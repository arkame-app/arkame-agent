//go:build windows

package cli

import "github.com/arkame-app/agent/internal/segredo"

// pastaAberta: no Windows a pasta que já existia herda a lista de permissões
// de cima (na raiz de um disco, Usuários Autenticados costumam poder alterar),
// e conferir cada entrada não é mais simples que fechar: trata como aberta.
func pastaAberta(string) (bool, error) { return true, nil }

// fecharPasta: a DACL protegida de Administradores e SYSTEM, herdada pelo que
// o RustFS gravar dentro.
func fecharPasta(p string) error { return segredo.ProtegerPasta(p) }
