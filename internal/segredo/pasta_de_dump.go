package segredo

import (
	"fmt"
	"os"
	"path/filepath"
)

// PastaDeDump prepara a pasta em que um comando de antes do backup grava o
// dump de um banco de dados: o banco inteiro, legível por quem ler a pasta.
//
// É PastaPrivada (criada fechada, só para o administrador; se já existia, de
// dono confiável e sem ser junção nem link) e mais duas coisas:
//
//   - O que já estiver dentro também tem de ser do administrador. Entre criar a
//     pasta e fechá-la há um instante em que ela herda a permissão da pasta de
//     cima (no Windows, %ProgramData% deixa qualquer usuário criar arquivos):
//     um `.bak` plantado ali nesse instante, com permissão para o seu dono,
//     seria reaproveitado pelo SQL Server (`WITH INIT` regrava o conteúdo e
//     mantém a permissão do arquivo) e lido pelo usuário que o plantou.
//   - escritor, quando há (só no Windows), é a conta que grava o dump e não é a
//     do agente: o SQL Server grava o `.bak` com a conta do serviço dele
//     (`NT Service\MSSQLSERVER`). Ela ganha modificar na pasta; o resto
//     continua só para Administradores e SYSTEM.
//
// A receita do SQL Server no painel chamava `icacls` pelo `cmd`, e um usuário
// comum que criasse `C:\ArkameDumps` antes ficava dono dela: o `icacls` não
// troca o dono, e o dono devolve a permissão a si mesmo (revisão do S14).
func PastaDeDump(dir, escritor string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("a pasta do dump tem de ser um caminho absoluto: %s", dir)
	}
	// Antes de criar qualquer coisa: a primeira pasta que já existe e as de
	// cima. Depois, de novo, com a do dump criada.
	if err := acimaDeConfianca(primeiraQueExiste(filepath.Dir(dir))); err != nil {
		return err
	}
	if err := PastaPrivada(dir); err != nil {
		return err
	}
	// Mais do que a pasta do estado: nenhuma pasta acima pode ser junção ou
	// link (Windows), e todas têm de ser de dono confiável — quem é dono de
	// uma delas renomeia a de baixo e põe a sua no lugar entre um dump e
	// outro. (Na pasta do estado isso não é exigido: instalações com
	// %ProgramData% redirecionado continuam funcionando.)
	if err := acimaDeConfianca(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := conteudoDeConfianca(dir); err != nil {
		return err
	}
	if escritor == "" {
		return nil
	}
	if err := permitirEscrita(dir, escritor); err != nil {
		return err
	}
	// De novo depois de fechar: um arquivo criado entre a primeira conferência
	// e a troca da permissão (numa pasta que já existia com permissão larga)
	// ficaria com a permissão explícita de quem o criou.
	return conteudoDeConfianca(dir)
}

// conteudoDeConfianca recusa link, junção ou arquivo de outro dono na pasta.
func conteudoDeConfianca(dir string) error {
	entradas, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entradas {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%w: %s é um link ou uma junção", ErrDonoNaoConfiavel, p)
		}
		if err := ConferirDono(p); err != nil {
			return fmt.Errorf("%w — apague-o e rode de novo", err)
		}
	}
	return nil
}

// primeiraQueExiste sobe de p até a primeira pasta que existe.
func primeiraQueExiste(p string) string {
	for ; filepath.Dir(p) != p; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
	}
	return p
}

// acimaDeConfianca confere cada pasta de p até a raiz do volume (fora ela):
// pasta de verdade, sem link no Windows, e de dono confiável.
func acimaDeConfianca(p string) error {
	if err := semLinkAcima(p); err != nil {
		return err
	}
	for ; filepath.Dir(p) != p; p = filepath.Dir(p) {
		if err := ConferirDono(p); err != nil {
			return err
		}
	}
	return nil
}
