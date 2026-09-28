// Package aplicativos põe o agente onde o sistema mostra o que está instalado
// e o tira de lá, junto com o próprio programa.
//
// No Windows o agente não aparecia em "Aplicativos instalados": quem queria
// removê-lo não tinha por onde (teste do fundador, 27/09). A entrada no
// registro chama `arkame-agent uninstall`, o mesmo comando dos outros
// sistemas.
package aplicativos
