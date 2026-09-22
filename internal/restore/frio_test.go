package restore

import (
	"errors"
	"strings"
	"testing"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// erroDeApi finge a resposta de erro que o SDK entrega quando a S3 devolve um
// código que ele não tem tipo próprio para representar.
func erroDeApi(codigo string) error {
	return &smithy.GenericAPIError{Code: codigo, Message: codigo}
}

// O defeito: o código catava `ObjectAlreadyInActiveTierError` (que significa
// "o objeto já está quente") acreditando estar catando
// `RestoreAlreadyInProgress` — que o SDK **não** expõe como tipo. Resultado: uma
// espera legítima virava item falhado, na janela entre o nosso RestoreObject e o
// cabeçalho x-amz-restore aparecer.
func TestJaEstaResolvendo(t *testing.T) {
	casos := []struct {
		nome   string
		err    error
		espera bool
	}{
		{"restauração já em andamento (sem tipo no SDK)", erroDeApi("RestoreAlreadyInProgress"), true},
		{"objeto já está quente (com tipo no SDK)", &s3types.ObjectAlreadyInActiveTierError{}, true},
		{"código só no texto, provedor compatível", errors.New("api error RestoreAlreadyInProgress: em andamento"), true},
		{"credencial errada não é espera", erroDeApi("AccessDenied"), false},
		{"objeto sumiu não é espera", erroDeApi("NoSuchKey"), false},
		{"erro de rede não é espera", errors.New("dial tcp: connection refused"), false},
	}
	for _, c := range casos {
		if got := jaEstaResolvendo(c.err); got != c.espera {
			t.Errorf("%s: jaEstaResolvendo = %v, queria %v", c.nome, got, c.espera)
		}
	}
}

// A previsão é teto publicado pela AWS, não média: quem restaura backup prefere
// que a conta feche antes do prometido.
func TestPrevisaoDeAquecimento(t *testing.T) {
	casos := []struct {
		classe string
		tier   s3types.Tier
		quanto time.Duration
	}{
		{"GLACIER", s3types.TierStandard, 5 * time.Hour},
		{"DEEP_ARCHIVE", s3types.TierStandard, 12 * time.Hour},
		{"GLACIER", s3types.TierBulk, 12 * time.Hour},
		{"DEEP_ARCHIVE", s3types.TierBulk, 48 * time.Hour},
		{"GLACIER", s3types.TierExpedited, 5 * time.Minute},
		// Classe desconhecida cai no caso menos otimista entre os não-Deep.
		{"", s3types.TierStandard, 5 * time.Hour},
	}
	for _, c := range casos {
		got := previsaoDeAquecimento(c.classe, c.tier)
		querido := time.Now().UTC().Add(c.quanto)
		if diff := got.Sub(querido); diff > time.Minute || diff < -time.Minute {
			t.Errorf("classe=%q tier=%v: previu %v, queria ~%v (diferença %v)",
				c.classe, c.tier, got, querido, diff)
		}
		if got.Before(time.Now()) {
			t.Errorf("classe=%q tier=%v: previsão no passado", c.classe, c.tier)
		}
	}
}

// A previsão precisa sair em RFC3339, que é o que a rota do painel aceita.
func TestPrevisaoSerializa(t *testing.T) {
	s := previsaoDeAquecimento("DEEP_ARCHIVE", s3types.TierStandard).Format(time.RFC3339)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Fatalf("previsão não volta de RFC3339: %v (%q)", err, s)
	}
	if !strings.HasSuffix(s, "Z") {
		t.Errorf("previsão deveria sair em UTC, saiu %q", s)
	}
}

// A exceção do Intelligent-Tiering: para as camadas de arquivo dele a AWS recusa
// `Days` e `GlacierJobParameters`. Regra de contrato de terceiro que não podemos
// medir (90 dias para um objeto chegar lá) — então ao menos fica travada aqui.
func TestPedidoDeRestauracao(t *testing.T) {
	comum := pedidoDeRestauracao("GLACIER", s3types.TierStandard)
	if comum.Days == nil || *comum.Days != 7 {
		t.Errorf("Glacier: Days = %v, queria 7 (a OCI só aceita de 1 a 10)", comum.Days)
	}
	if comum.GlacierJobParameters == nil || comum.GlacierJobParameters.Tier != s3types.TierStandard {
		t.Error("Glacier: faltou GlacierJobParameters com o tier")
	}

	deep := pedidoDeRestauracao("DEEP_ARCHIVE", s3types.TierStandard)
	if deep.Days == nil || deep.GlacierJobParameters == nil {
		t.Error("Deep Archive deveria mandar os mesmos parâmetros do Glacier")
	}

	it := pedidoDeRestauracao("INTELLIGENT_TIERING", s3types.TierStandard)
	if it.Days != nil {
		t.Errorf("Intelligent-Tiering: Days deveria ir ausente, foi %v", *it.Days)
	}
	if it.GlacierJobParameters != nil {
		t.Error("Intelligent-Tiering: GlacierJobParameters deveria ir ausente")
	}

	// Classe vazia é o caso da OCI, cujo HeadObject não informa a classe. Ela
	// exige Days, então o pedido comum é o certo.
	oci := pedidoDeRestauracao("", s3types.TierStandard)
	if oci.Days == nil || *oci.Days < 1 || *oci.Days > 10 {
		t.Errorf("classe vazia (OCI): Days = %v, precisa estar entre 1 e 10", oci.Days)
	}
}
