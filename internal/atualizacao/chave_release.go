//go:build release

package atualizacao

// Se o build de release parou aqui ("constant -44 overflows uint" ou parecido):
// a constante chavePublicaAtual, em chaves.go, está vazia ou não tem os 44
// caracteres de uma chave Ed25519 em base64. Um release sem ela sairia com um
// agente que não confia em atualização nenhuma — e os agentes instalados não
// aceitariam o release, que não teria assinatura que eles conheçam. Gere a
// chave e preencha a constante: docs/RELEASE-ASSINATURA.md.
const (
	_ = uint(len(chavePublicaAtual) - tamanhoDaChaveEmBase64)
	_ = uint(tamanhoDaChaveEmBase64 - len(chavePublicaAtual))
)

// No release não há chave de teste: só as de produção valem.
func chavesDeTeste() []string { return nil }
