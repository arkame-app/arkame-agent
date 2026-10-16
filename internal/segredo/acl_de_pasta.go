package segredo

import "fmt"

// Uma pasta acima da do dump, mesmo de dono confiável, pode deixar outra
// conta trocá-la de lugar. A pasta que o administrador cria pelo Explorer na
// raiz do C: herda "Modificar" para Usuários Autenticados — e modificar
// inclui apagar, logo renomear. Entre o `prepare-dump-folder` e o `BACKUP
// DATABASE`, um usuário comum renomeia `C:\Dumps`, cria a sua `C:\Dumps\sql`
// aberta, e o SQL Server grava o banco inteiro nela (revisão do S14, 7ª
// passada). A conferência do dono não pega isso: o dono é Administradores.
//
// Por isso, além do dono, a permissão de cada pasta acima: nenhuma conta fora
// das confiáveis pode apagar (renomear) a pasta, mudar a permissão ou o dono
// dela, nem apagar o que está dentro (renomear a de baixo). Na raiz do volume
// só os dois últimos contam: a raiz não se renomeia.

// Os direitos de uma ACE que entregam a pasta (máscara de acesso do Windows).
const (
	direitoApagar       = 0x00010000 // DELETE: renomear a própria pasta
	direitoMudarDACL    = 0x00040000 // WRITE_DAC
	direitoMudarDono    = 0x00080000 // WRITE_OWNER
	direitoTudo         = 0x10000000 // GENERIC_ALL
	direitoApagarDentro = 0x00000040 // FILE_DELETE_CHILD: renomear o que está dentro
)

// Tipos e marcas de ACE que importam aqui.
const (
	aceDePermitir         = 0  // ACCESS_ALLOWED_ACE_TYPE
	aceDePermitirDeObjeto = 5  // ACCESS_ALLOWED_OBJECT_ACE_TYPE (SID noutra posição)
	aceDePermitirCondicao = 9  // ACCESS_ALLOWED_CALLBACK_ACE_TYPE (mesmo formato da 0)
	aceDePermitirCondObj  = 11 // ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE
	aceSoParaHerdar       = 0x08
)

// sidDoTrustedInstaller é o dono das pastas do próprio Windows.
const sidDoTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// SIDs que podem ter qualquer direito nas pastas acima: SYSTEM,
// Administradores, TrustedInstaller, e os dois marcadores que valem pelo dono
// (CREATOR OWNER só se aplica ao herdar; OWNER RIGHTS é o dono, já conferido).
var sidsConfiaveisFixos = map[string]bool{
	"S-1-5-18":            true,
	"S-1-5-32-544":        true,
	sidDoTrustedInstaller: true,
	"S-1-3-0":             true,
	"S-1-3-4":             true,
}

// aceLida é uma ACE da DACL de uma pasta, já fora da memória do Windows.
type aceLida struct {
	Tipo    uint8
	Marcas  uint8
	Mascara uint32
	SID     string
}

// aceQueEntrega diz se a ACE dá a uma conta fora das confiáveis um direito
// que troca a pasta (ou o que está dentro dela) de lugar. A ACE de negar não
// conta a favor: na dúvida, recusa-se — o administrador tira o direito, ou usa
// a pasta padrão.
func aceQueEntrega(a aceLida, raiz bool, confiavel func(sid string) bool) (bool, error) {
	if a.Marcas&aceSoParaHerdar != 0 {
		return false, nil // vale só para o que for criado dentro, não para a pasta
	}
	switch a.Tipo {
	case aceDePermitir, aceDePermitirCondicao:
	case aceDePermitirDeObjeto, aceDePermitirCondObj:
		return true, fmt.Errorf("permissão de tipo %d, que o agente não sabe ler", a.Tipo)
	default:
		return false, nil
	}
	if confiavel(a.SID) {
		return false, nil
	}
	perigo := uint32(direitoMudarDACL | direitoMudarDono | direitoTudo | direitoApagarDentro)
	if !raiz {
		perigo |= direitoApagar
	}
	return a.Mascara&perigo != 0, nil
}

// comPrefixoLongo põe o prefixo `\\?\` num caminho absoluto do Windows, para
// a chamada direta à API aceitar mais de 260 caracteres (o pacote os faz isso
// sozinho; CreateDirectory e CreateFile, não): `C:\x` vira `\\?\C:\x`,
// `\\srv\share\x` vira `\\?\UNC\srv\share\x` e `\\.\C:\x` vira `\\?\C:\x`.
// Com o prefixo o Windows não normaliza mais nada — quem chama passa o
// caminho já limpo (filepath.Clean).
func comPrefixoLongo(p string) string {
	switch {
	case len(p) >= 4 && p[:4] == `\\?\`:
		return p
	case len(p) >= 4 && p[:4] == `\\.\`:
		return `\\?\` + p[4:]
	case len(p) >= 2 && p[:2] == `\\`:
		return `\\?\UNC\` + p[2:]
	case len(p) >= 3 && p[1] == ':' && p[2] == '\\':
		return `\\?\` + p
	}
	return p
}
