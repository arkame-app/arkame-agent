package service

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// plistTmpl monta o job do launchd. KeepAlive mantém o agent de pé; RunAtLoad
// sobe junto com o sistema (LaunchDaemon) ou com o login (LaunchAgent).
//
// O launchd não tem EnvironmentFile: o env-file é lido pelo próprio agent via
// --config, e o plist só precisa apontar para ele. As variáveis de ambiente
// dizem ao processo sob qual label e escopo ele roda (ver detectLaunchd).
//
// ExitTimeOut é EsperaParada (segundosDeParada): é quanto o launchd espera
// depois do SIGTERM antes do SIGKILL. O padrão, de ~20s, matava a finalização
// do daemon (comando de depois, /fail ou /complete, PATCH da restauração).
//
// Fora de build tag para o teste rodar em qualquer plataforma.
const plistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[1]s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[2]s</string>
		<string>run</string>
		<string>--config</string>
		<string>%[3]s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + envNomeDoServico + `</key>
		<string>%[1]s</string>
		<key>` + envEscopoDoServico + `</key>
		<string>%[5]s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>%[6]d</integer>
	<key>StandardOutPath</key>
	<string>%[4]s</string>
	<key>StandardErrorPath</key>
	<string>%[4]s</string>
</dict>
</plist>
`

// montarPlist gera o plist do job com o label, o binário, o env-file, o log e
// o escopo (system/user) da instalação.
func montarPlist(label, binario, configPath, logPath string, escopo Scope) string {
	return fmt.Sprintf(plistTmpl,
		xmlEscape(label), xmlEscape(binario), xmlEscape(configPath), xmlEscape(logPath), xmlEscape(string(escopo)),
		segundosDeParada())
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
