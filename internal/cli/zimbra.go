package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/zimbra"
)

// newZimbraCmd: o backup das contas de um servidor Zimbra. O `export` é o
// comando de antes do plano (a receita "Zimbra" do painel); o `restore`
// devolve uma conta a partir da pasta que o backup restaurou.
func newZimbraCmd() *cobra.Command {
	var pasta string
	cmd := &cobra.Command{
		Use:   "zimbra",
		Short: "Backup e restauração das contas de um servidor Zimbra",
	}
	cmd.PersistentFlags().StringVar(&pasta, "zimbra-dir", zimbra.PastaPadrao, "pasta onde o Zimbra está instalado")
	conectar := func(ctx context.Context) (*zimbra.Cliente, error) {
		if err := zimbra.Instalado(pasta); err != nil {
			return nil, err
		}
		return zimbra.Conectar(ctx, pasta)
	}

	listar := &cobra.Command{
		Use:   "list",
		Short: "Lista as contas que o backup protege (as de pessoas; as do sistema ficam de fora)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conectar(cmd.Context())
			if err != nil {
				return err
			}
			contas, err := c.ListarContas(cmd.Context())
			if err != nil {
				return err
			}
			for _, c := range contas {
				if c.Motivo != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  (fica de fora: %s)\n", c.Nome, c.Motivo)
					continue
				}
				fmt.Fprintln(cmd.OutOrStdout(), c.Nome)
			}
			return nil
		},
	}

	var o zimbra.Opcoes
	var contas, excluir string
	exportar := &cobra.Command{
		Use:   "export",
		Short: "Exporta as contas (mensagens por mês, configuração, filtros e assinaturas) para a pasta do backup",
		Long: `Exporta cada conta para <dir>/contas/<conta>/: as mensagens num arquivo por mês
(caixa-AAAA-MM.tgz, de todas as pastas, Lixeira e Spam inclusive; o mês
fechado não é exportado de novo), os contatos, a agenda, as tarefas e os
documentos (estrutura.tgz), a configuração (conta.json; a senha não vai), os
filtros (filtros.sieve, filtros-saida.sieve) e as assinaturas
(assinaturas.json). Grava também <dir>/inventario.json, com as contas, os
meses e os tamanhos; a conta que falhou fica com o arquivo exportacao-falhou.

--years 0 leva todas as mensagens (os últimos 24 meses mês a mês, e o que é mais
antigo em anteriores.tgz); --years N só as dos últimos N anos (os meses de fora
saem da pasta). Uma conta que falha não para as outras: aparece no resumo.

Dentro de um plano, o comando para antes do prazo (ARKAME_HOOK_DEADLINE) e sai
com sucesso: o que faltou continua na próxima sessão, começando pelas contas
há mais tempo sem exportação completa.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !filepath.IsAbs(o.Pasta) {
				return fmt.Errorf("--dir tem de ser um caminho absoluto")
			}
			o.Pasta = filepath.Clean(o.Pasta)
			o.Contas, o.Excluir = lista(contas), lista(excluir)
			if s := os.Getenv("ARKAME_HOOK_DEADLINE"); s != "" {
				n, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					return fmt.Errorf("ARKAME_HOOK_DEADLINE inválido: %q", s)
				}
				o.Prazo = time.Unix(n, 0)
			}
			c, err := conectar(cmd.Context())
			if err != nil {
				return err
			}
			_, err = zimbra.Exportar(cmd.Context(), c, o, cmd.OutOrStdout())
			return err
		},
	}
	exportar.Flags().StringVar(&o.Pasta, "dir", "", "pasta da exportação (entra no plano)")
	exportar.Flags().IntVar(&o.Anos, "years", 0, "0 = todas as mensagens; N = só as dos últimos N anos")
	exportar.Flags().StringVar(&contas, "accounts", "", "só estas contas, separadas por vírgula (padrão: todas)")
	exportar.Flags().StringVar(&excluir, "exclude", "", "menos estas contas, separadas por vírgula")
	_ = exportar.MarkFlagRequired("dir")

	var r zimbra.OpcoesDeRestauracao
	restaurar := &cobra.Command{
		Use:   "restore",
		Short: "Restaura uma conta a partir da pasta que o backup restaurou",
		Long: `Restaura uma conta a partir da pasta dela, como o painel a restaurou
(…/contas/<conta>) e importa as mensagens, os contatos e a agenda.

Se a conta não existe, ela é recriada com a configuração, a classe de
serviço, os apelidos, os filtros e as assinaturas de antes, e uma senha
aleatória (a senha não vai para o backup: defina outra depois). Se a conta
existe, só as mensagens voltam; --settings reaplica também a configuração (a
senha nunca). O que manda e-mail para fora (encaminhamento, resposta
automática, aviso de mensagem nova) só volta com --with-forwarding.

--resolve diz o que fazer com o item que já está na caixa: skip (padrão, não
mexe no que está lá), modify, replace, ou reset (APAGA a caixa antes de
importar). --to restaura as mensagens noutra conta; se ela não existe, é criada
vazia, com uma senha aleatória (sem a configuração da original).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !filepath.IsAbs(r.De) {
				return fmt.Errorf("--from tem de ser um caminho absoluto")
			}
			c, err := conectar(cmd.Context())
			if err != nil {
				return err
			}
			return zimbra.Restaurar(cmd.Context(), c, r, cmd.OutOrStdout())
		},
	}
	restaurar.Flags().StringVar(&r.De, "from", "", "pasta da conta restaurada (…/contas/<conta>)")
	restaurar.Flags().StringVar(&r.Para, "to", "", "conta que recebe as mensagens (padrão: a mesma)")
	restaurar.Flags().StringVar(&r.Resolver, "resolve", "skip", "skip, modify, replace ou reset")
	restaurar.Flags().BoolVar(&r.SemMensagens, "settings-only", false, "só a configuração, os filtros e as assinaturas")
	restaurar.Flags().BoolVar(&r.Configuracao, "settings", false, "numa conta que existe, reaplica também a configuração, os filtros e as assinaturas")
	restaurar.Flags().BoolVar(&r.Encaminhamentos, "with-forwarding", false, "reaplica também encaminhamento, resposta automática e aviso de mensagem nova")
	_ = restaurar.MarkFlagRequired("from")

	cmd.AddCommand(listar, exportar, restaurar)
	return cmd
}

func lista(s string) []string {
	var r []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			r = append(r, p)
		}
	}
	return r
}
