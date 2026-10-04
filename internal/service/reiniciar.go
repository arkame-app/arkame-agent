package service

import (
	"errors"
	"strings"
	"time"
)

// EsperaParada é quanto Reiniciar e Parar esperam o serviço parar depois de
// pedir a parada. Cobre a finalização do daemon, que depois do Stop ainda
// roda até três etapas em sequência com até finalizacaoGraca (30s) cada —
// comando de depois, /fail ou /complete, PATCH final da restauração —, mais
// folga para o processo sair. Com 60s, um backup no fim estourava a espera e
// o serviço parava sozinho depois, sem ninguém o iniciar de novo. O teste do
// daemon confere que continua cobrindo 3 × finalizacaoGraca.
const EsperaParada = 150 * time.Second

// ErrNaoParou: o serviço recebeu o pedido de parada e não parou em
// EsperaParada. Ele pode parar logo depois e ficar parado — o SCM só religa
// serviço que caiu —, então quem avisa não pode dizer que ele segue rodando.
var ErrNaoParou = errors.New("o serviço não parou a tempo e pode ficar parado")

// servicoRegistrado é um serviço do agente como o registro do SO o descreve
// (unit do systemd, plist do launchd): o nome, o escopo e o programa que ele
// chama. É o que RodandoOPrograma e Reiniciar consultam fora do Windows.
type servicoRegistrado struct {
	nome     string
	escopo   Scope
	programa string
}

// doPrograma devolve, sem repetir, o nome de cada serviço registrado que chama
// o programa exe (já resolvido) e está rodando agora. rodando só é consultado
// para quem chama o programa: no systemd é um `systemctl is-active` por unit.
func doPrograma(goos string, todos []servicoRegistrado, exe string, rodando func(servicoRegistrado) bool) []string {
	var nomes []string
	visto := map[string]bool{}
	for _, s := range todos {
		chave := string(s.escopo) + " " + chaveDoServico(goos, s.nome)
		if visto[chave] || !mesmoPrograma(goos, s.programa, exe) || !rodando(s) {
			continue
		}
		visto[chave] = true
		nomes = append(nomes, s.nome)
	}
	return nomes
}

// escolherRegistro acha o registro do serviço nome para reiniciá-lo: o
// escopo vem dele. Com o mesmo nome nos dois escopos, vale o que está rodando.
func escolherRegistro(goos string, todos []servicoRegistrado, nome string, rodando func(servicoRegistrado) bool) (servicoRegistrado, bool) {
	var achados []servicoRegistrado
	for _, s := range todos {
		if chaveDoServico(goos, s.nome) == chaveDoServico(goos, nome) {
			achados = append(achados, s)
		}
	}
	if len(achados) == 0 {
		return servicoRegistrado{}, false
	}
	if len(achados) > 1 {
		for _, s := range achados {
			if rodando(s) {
				return s, true
			}
		}
	}
	return achados[0], true
}

// programaDaUnit devolve o programa do ExecStart de uma unit do systemd (o
// primeiro não vazio: um `ExecStart=` vazio só zera a lista). Os prefixos do
// systemd (-, @, :, +, !) vêm antes do caminho, e %% é um %.
func programaDaUnit(conteudo string) string {
	for _, l := range strings.Split(conteudo, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(l), "ExecStart=")
		if !ok {
			continue
		}
		v = strings.ReplaceAll(strings.TrimLeft(strings.TrimSpace(v), "-@:+!"), "%%", "%")
		if p := programaDaLinha(v); p != "" {
			return p
		}
	}
	return ""
}

// launchdRodando lê a saída de `launchctl print <alvo>`: o job está rodando
// quando o estado é running.
func launchdRodando(saida string) bool {
	for _, l := range strings.Split(saida, "\n") {
		if strings.TrimSpace(l) == "state = running" {
			return true
		}
	}
	return false
}
