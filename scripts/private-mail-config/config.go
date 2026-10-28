// private-mail-config prepares and verifies native configuration; public mail
// activation remains a separate deployment step.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var errConfiguration = errors.New("email configuration is incomplete or invalid")

type settings struct {
	Domain               string   `json:"domain"`
	Hostname             string   `json:"hostname"`
	Maintainer           string   `json:"maintainer"`
	Agent                string   `json:"agent"`
	Administrator        string   `json:"administrator"`
	MaintainerAliases    []string `json:"maintainerAliases,omitempty"`
	UseForwardedClientIP bool     `json:"useForwardedClientIP,omitempty"`
	UseProxyClientIP     bool     `json:"useProxyClientIP,omitempty"`
}
type inputs struct {
	settings
	administratorPassword, maintainerPassword, agentPassword string
}

var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var localPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)
var passwordPattern = regexp.MustCompile(`^[a-zA-Z0-9]{40,128}$`)

// render builds staged settings and account changes from validated private inputs.
func render(v inputs) ([]byte, error) {
	if v.UseForwardedClientIP && v.UseProxyClientIP {
		return nil, errConfiguration
	}
	if !domainPattern.MatchString(v.Domain) || strings.HasSuffix(v.Domain, ".invalid") || len(v.Domain) > 200 || !domainPattern.MatchString(v.Hostname) || !strings.HasSuffix(v.Hostname, "."+v.Domain) || len(v.Hostname) > 253 {
		return nil, errConfiguration
	}
	seen := map[string]bool{}
	for _, name := range []string{v.Maintainer, v.Agent, v.Administrator} {
		if !localPattern.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".") || seen[name] {
			return nil, errConfiguration
		}
		seen[name] = true
	}
	if len(v.MaintainerAliases) > 16 {
		return nil, errConfiguration
	}
	for _, name := range v.MaintainerAliases {
		if !localPattern.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".") || seen[name] {
			return nil, errConfiguration
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for _, password := range []string{v.administratorPassword, v.maintainerPassword, v.agentPassword} {
		if !passwordPattern.MatchString(password) || seen[password] {
			return nil, errConfiguration
		}
		seen[password] = true
	}
	// Expression values contain only the validated conservative address alphabet.
	applicationDigest, err := pinnedApplicationDigest()
	if err != nil {
		return nil, errConfiguration
	}
	agent := v.Agent + "@" + v.Domain
	recipient := v.Maintainer + "@" + v.Domain
	expression := func(value string) map[string]any { return map[string]any{"match": map[string]any{}, "else": value} }
	account := func(name, password, role string) map[string]any {
		return map[string]any{"@type": "User", "name": name, "domainId": "#domain", "credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}}, "roles": map[string]any{"@type": role}}
	}
	administrator := account(v.Administrator, v.administratorPassword, "Admin")
	// Replaying the local baseline must recover its managed administrator even
	// after a later identity cutover has suspended the ordinary account.
	administrator["permissions"] = map[string]any{"@type": "Inherit"}
	maintainer := account(v.Maintainer, v.maintainerPassword, "User")
	if v.MaintainerAliases != nil {
		maintainer["aliases"] = receivingAliases(v.MaintainerAliases, "#domain")
	}
	agents := account(v.Agent, v.agentPassword, "User")
	permissions := map[string]bool{}
	for _, name := range []string{"authenticate", "emailSend", "emailReceive", "jmapMailboxGet", "jmapMailboxChanges", "jmapMailboxQuery", "jmapMailboxQueryChanges", "jmapThreadGet", "jmapThreadChanges", "jmapEmailGet", "jmapEmailChanges", "jmapEmailQuery", "jmapEmailQueryChanges", "jmapBlobGet"} {
		permissions[name] = true
	}
	listener := func(name, protocol, bind string, tls, implicit bool) map[string]any {
		return map[string]any{"name": name, "protocol": protocol, "bind": map[string]bool{bind: true}, "useTls": tls, "tlsImplicit": implicit}
	}
	clientProxy := listener("client-proxy", "http", "127.0.0.1:18081", false, false)
	proxyTrust := map[string]bool{}
	if v.UseProxyClientIP {
		proxyTrust["127.0.0.1"] = true
	}
	// Only this dedicated local listener consumes a transport identity. Do not
	// exempt a shared intermediary through global proxy or allowed-IP settings.
	clientProxy["overrideProxyTrustedNetworks"] = proxyTrust
	upsert := func(object string, match []string, value any) map[string]any {
		return map[string]any{"@type": "upsert", "object": object, "matchOn": match, "value": value}
	}
	update := func(object string, value any) map[string]any {
		return map[string]any{"@type": "update", "object": object, "value": value}
	}
	operations := []map[string]any{
		// Match the tagged variant so replay keeps the same console tracer. A
		// second matching tracer is refused by the native CLI, never chosen by age.
		upsert("Tracer", []string{"@type"}, map[string]any{"email-console": map[string]any{
			"@type": "Stdout", "enable": true, "level": "info", "lossy": false,
			"buffered": false, "ansi": false, "multiline": false, "eventsPolicy": "include",
			"events": map[string]bool{
				"server.startup": true, "server.shutdown": true, "server.startup-error": true, "server.thread-error": true,
				"network.listen-start": true, "network.listen-stop": true, "network.listen-error": true, "network.bind-error": true,
				"resource.error": true, "resource.not-found": true, "resource.bad-parameters": true, "resource.application-unpacked": true,
				"store.rocksdb-error": true,
			},
		}}),
		// Declare the existing local authentication state explicitly. A main
		// recovery must clear a later external directory rather than retain it.
		update("Authentication", map[string]any{"directoryId": nil}),
		upsert("Domain", []string{"name"}, map[string]any{"domain": map[string]any{"name": v.Domain, "isEnabled": true, "allowRelaying": false, "catchAllAddress": nil, "subAddressing": map[string]any{"@type": "Disabled"}, "dkimManagement": map[string]any{"@type": "Automatic"}, "dnsManagement": map[string]any{"@type": "Manual"}}}),
		upsert("Account", []string{"name", "domainId"}, map[string]any{"administrator": administrator, "maintainer": maintainer, "agents": agents}),
		// Address the exact ID returned by the upsert and replace the whole
		// tagged permission object; a second upsert can reuse a stale lookup.
		map[string]any{"@type": "update", "object": "Account", "id": "#agents", "value": map[string]any{"permissions": map[string]any{"@type": "Replace", "enabledPermissions": permissions, "disabledPermissions": map[string]bool{}}}},
		upsert("Certificate", []string{"certificate"}, map[string]any{"mail-tls": map[string]any{
			"certificate": map[string]any{"@type": "File", "filePath": "/etc/stalwart/tls/tls.crt"},
			"privateKey":  map[string]any{"@type": "File", "filePath": "/etc/stalwart/tls/tls.key"},
		}}),
		update("SystemSettings", map[string]any{"defaultHostname": v.Hostname, "defaultDomainId": "#domain", "defaultCertificateId": "#mail-tls", "proxyTrustedNetworks": map[string]bool{}}),
		// Named listeners are prepared while the recovery-only process ignores them.
		// Public activation separately removes recovery mode and supplies TLS files.
		upsert("NetworkListener", []string{"name"}, map[string]any{
			"client-proxy": clientProxy,
			"management":   listener("management", "http", "0.0.0.0:8080", false, false),
			"inbound":      listener("smtp-inbound", "smtp", "0.0.0.0:1025", true, false),
			"submission":   listener("submission", "smtp", "0.0.0.0:1587", true, false),
			"imap":         listener("imaps", "imap", "0.0.0.0:1993", true, true),
		}),
		update("MtaStageAuth", map[string]any{
			"saslMechanisms": map[string]any{"match": map[string]any{"0": map[string]any{"if": "listener == 'submission' && is_tls", "then": "[plain, login]"}}, "else": "false"},
			"require":        expression("listener == 'submission'"), "mustMatchSender": expression("true"),
		}),
		update("MtaStageMail", map[string]any{
			"isSenderAllowed": expression("listener != 'submission' || authenticated_as != ''"),
			"rewrite":         expression("false"), "script": expression("false"),
		}),
		update("MtaStageRcpt", map[string]any{
			"rewrite": map[string]any{"match": map[string]any{
				"0": map[string]any{"if": "authenticated_as == '" + agent + "' && rcpt == '" + recipient + "'", "then": "'" + recipient + "'"},
				"1": map[string]any{"if": "authenticated_as == '" + agent + "'", "then": "'denied@example.invalid'"},
			}, "else": "false"},
			"allowRelaying": expression("authenticated_as != '' && (authenticated_as != '" + agent + "' || rcpt == '" + recipient + "')"),
			"script":        expression("false"),
		}),
		// The server's upload endpoint does not honor the JMAP mutation permission
		// list. This dedicated IMAP/SMTP service denies it for every account.
		update("Http", map[string]any{"allowedEndpoints": map[string]any{"match": map[string]any{"0": map[string]any{"if": "starts_with(path, '/jmap/upload/')", "then": "403"}}, "else": "200"}, "useXForwarded": v.UseForwardedClientIP, "usePermissiveCors": false}),
		upsert("Application", []string{"description"}, map[string]any{"webadmin": map[string]any{
			"description": "Stalwart Web Interface", "enabled": true,
			"resourceUrl": "file://" + applicationAssetPath(applicationDigest), "autoUpdateFrequency": int64(30 * 24 * 60 * 60 * 1000),
			"urlPrefix":       map[string]bool{"/admin": true, "/account": true},
			"unpackDirectory": nil, "oauthClientId": nil,
		}}),
		update("MtaStageData", map[string]any{"addAuthResultsHeader": expression("true"), "script": expression("false")}),
		update("SenderAuth", map[string]any{
			"dkimVerify":  map[string]any{"match": map[string]any{"0": map[string]any{"if": "listener == 'smtp-inbound'", "then": "'relaxed'"}}, "else": "false"},
			"dmarcVerify": map[string]any{"match": map[string]any{"0": map[string]any{"if": "listener == 'smtp-inbound'", "then": "'relaxed'"}}, "else": "false"},
			"dkimStrict":  true,
		}),
		update("Imap", map[string]any{"allowPlainTextAuth": false}),
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, operation := range operations {
		if encoder.Encode(operation) != nil {
			return nil, errConfiguration
		}
	}
	return output.Bytes(), nil
}
