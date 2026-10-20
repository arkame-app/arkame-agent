package zimbra

import (
	"bytes"
	"strings"
	"testing"
)

// Um Zimbra 10.1 que recusa o valor do 8.8.15 e um atributo que não conhece:
// a conta é criada com o equivalente e sem o desconhecido (teste de migração
// 8.8.15 → 10.1, 20/10/2026: com zimbrax, a conta não nascia e nenhuma
// mensagem voltava).
func TestComAtributosAceitosNaMigracao(t *testing.T) {
	as := []atributo{
		{N: "zimbraPrefClientType", V: "zimbrax"},
		{N: "zimbraPrefLocale", V: "pt_BR"},
		{N: "zimbraPrefSomethingOld", V: "x"},
	}
	var chamadas [][]atributo
	pedido := func(as []atributo) error {
		chamadas = append(chamadas, as)
		for _, a := range as {
			if a.N == "zimbraPrefClientType" && a.V != "modern" && a.V != "standard" && a.V != "advanced" {
				return &ErroDoZimbra{Codigo: "account.INVALID_ATTR_VALUE", Motivo: "zimbraPrefClientType must be one of: standard,advanced,modern"}
			}
			if a.N == "zimbraPrefSomethingOld" {
				// No CreateAccount do 10.1 o atributo desconhecido vem do LDAP.
				return &ErroDoZimbra{Codigo: "ldap.INVALID_ATTR_NAME", Motivo: "invalid attr name: zimbraPrefSomethingOld"}
			}
		}
		return nil
	}
	var log bytes.Buffer
	foram, recusados, err := comAtributosAceitos(as, &log, pedido)
	if err != nil {
		t.Fatalf("erro: %v (log: %s)", err, log.String())
	}
	if len(recusados) != 1 || recusados[0] != "zimbraPrefSomethingOld" {
		t.Fatalf("recusados = %v", recusados)
	}
	tem := map[string]string{}
	for _, a := range foram {
		tem[a.N] = a.V
	}
	if tem["zimbraPrefClientType"] != "modern" || tem["zimbraPrefLocale"] != "pt_BR" || len(foram) != 2 {
		t.Fatalf("foram = %v", foram)
	}
	if !strings.Contains(log.String(), "zimbrax") || !strings.Contains(log.String(), "zimbraPrefSomethingOld") {
		t.Fatalf("o log não avisa: %s", log.String())
	}
	if len(chamadas) != 3 {
		t.Fatalf("chamadas = %d, esperava 3", len(chamadas))
	}
}

// Erro que não é de atributo volta como veio, sem nova tentativa.
func TestComAtributosAceitosOutroErro(t *testing.T) {
	n := 0
	_, _, err := comAtributosAceitos([]atributo{{N: "zimbraPrefLocale", V: "pt_BR"}}, &bytes.Buffer{}, func([]atributo) error {
		n++
		return &ErroDoZimbra{Codigo: "account.ACCOUNT_EXISTS", Motivo: "email address already exists"}
	})
	if err == nil || n != 1 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

// Valor recusado sem equivalente, e que continua sendo recusado: sai, e não fica em laço.
func TestComAtributosAceitosSemLaco(t *testing.T) {
	n := 0
	foram, recusados, err := comAtributosAceitos([]atributo{{N: "zimbraPrefClientType", V: "zimbrax"}}, &bytes.Buffer{}, func(as []atributo) error {
		n++
		if len(as) > 0 {
			return &ErroDoZimbra{Codigo: "account.INVALID_ATTR_VALUE", Motivo: "zimbraPrefClientType must be one of: standard,advanced"}
		}
		return nil
	})
	if err != nil || len(foram) != 0 || len(recusados) != 1 || n != 2 {
		t.Fatalf("err=%v foram=%v recusados=%v n=%d", err, foram, recusados, n)
	}
}
