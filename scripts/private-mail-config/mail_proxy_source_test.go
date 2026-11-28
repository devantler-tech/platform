package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mailListenerTrust renders the plan for one load-balancer address and returns
// each NetworkListener's PROXY trust by listener name.
func mailListenerTrust(t *testing.T, source string) map[string]map[string]any {
	t.Helper()
	v, err := loadInputs(bootstrapInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	v.mailProxySource = source
	// As production deploys it, with implicit-TLS submission (--hostname).
	v.implicitSubmission = true
	plan, err := render(v)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	trust := map[string]map[string]any{}
	for _, op := range ops {
		if op.Object != "NetworkListener" || op.Type != "upsert" {
			continue
		}
		for _, raw := range op.Value {
			listener := raw.(map[string]any)
			networks, ok := listener["overrideProxyTrustedNetworks"].(map[string]any)
			if ok {
				trust[listener["name"].(string)] = networks
			}
		}
	}
	return trust
}

func TestMailListenersTrustOnlyTheirLoadBalancer(t *testing.T) {
	trust := mailListenerTrust(t, "10.0.0.250")
	for _, name := range []string{"smtp-inbound", "submission", "submissions", "imaps"} {
		if len(trust[name]) != 1 || trust[name]["10.0.0.250"] != true {
			t.Fatalf("%s must trust exactly the mail load balancer, got %v", name, trust[name])
		}
	}
	// The administration route keeps its own loopback identity, unchanged.
	if len(trust["client-proxy"]) != 0 {
		t.Fatalf("client-proxy trust changed with the mail address: %v", trust["client-proxy"])
	}
	if _, ok := trust["management"]; ok {
		t.Fatal("the management listener gained a PROXY trust")
	}
}

// Removing the address must remove the trust, so the empty trust is written.
func TestMailListenersTrustNothingWithoutAnAddress(t *testing.T) {
	trust := mailListenerTrust(t, "")
	for _, name := range []string{"smtp-inbound", "submission", "submissions", "imaps"} {
		networks, ok := trust[name]
		if !ok || len(networks) != 0 {
			t.Fatalf("%s must carry an explicit empty trust, got %v (present=%v)", name, networks, ok)
		}
	}
}

func TestMailProxySourceAcceptsOnlyOnePrivateIPv4Address(t *testing.T) {
	for value, want := range map[string]bool{
		"10.0.0.250":      true,
		"172.16.4.9":      true,
		"192.168.1.20":    true,
		"":                false,
		"5.75.215.250":    false, // public
		"127.0.0.1":       false, // loopback
		"0.0.0.0":         false,
		"10.0.0.0/16":     false,
		"10.0.0.250/32":   false,
		"10.000.0.250":    false,
		"::ffff:10.0.0.1": false,
		"fd00::1":         false,
		"10.0.0.250 ":     false,
		"mail.local":      false,
	} {
		var o mailProxySourceOption
		if got := o.Set(value) == nil; got != want {
			t.Errorf("Set(%q) accepted=%v want=%v", value, got, want)
		}
	}
}

func TestRenderRefusesAnInvalidMailProxySource(t *testing.T) {
	v, err := loadInputs(bootstrapInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	v.mailProxySource = "5.75.215.250"
	if _, err := render(v); err == nil {
		t.Fatal("a public address was accepted as the mail load balancer")
	}
}

// Every command that renders or checks the plan takes the same flag and refuses
// an invalid address without repeating it.
func TestMailProxySourceFlagIsValidatedWithoutDisclosure(t *testing.T) {
	for name, run := range map[string]func([]string, *bytes.Buffer) int{
		"bootstrap": func(args []string, log *bytes.Buffer) int { return bootstrapCommand(args, log) },
		"check":     func(args []string, log *bytes.Buffer) int { return checkCommand(args, log) },
		"plan":      func(args []string, log *bytes.Buffer) int { return command(args, log) },
	} {
		var log bytes.Buffer
		if run([]string{"--mail-proxy-source=203.0.113.77"}, &log) == 0 || strings.Contains(log.String(), "203.0.113.77") {
			t.Fatalf("%s accepted or disclosed an invalid mail load balancer address: %q", name, log.String())
		}
	}
}

func TestMailListenerInventoryAcceptsOnlyOnePrivateLoadBalancerTrust(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listener string
		trust    map[string]bool
		want     bool
	}{
		{"none", "imaps", map[string]bool{}, true},
		{"load balancer", "imaps", map[string]bool{"10.0.0.250": true}, true},
		{"load balancer as /32", "smtp-inbound", map[string]bool{"10.0.0.250/32": true}, true},
		{"submission", "submission", map[string]bool{"10.0.0.250": true}, true},
		{"implicit-TLS submission", "submissions", map[string]bool{"10.0.0.250": true}, true},
		{"implicit-TLS submission wider network", "submissions", map[string]bool{"10.0.0.0/16": true}, false},
		{"public address", "imaps", map[string]bool{"5.75.215.250": true}, false},
		{"wider network", "imaps", map[string]bool{"10.0.0.0/16": true}, false},
		{"loopback", "imaps", map[string]bool{"127.0.0.1": true}, false},
		{"two addresses", "imaps", map[string]bool{"10.0.0.250": true, "10.0.0.251": true}, false},
		{"false entry", "imaps", map[string]bool{"10.0.0.250": false}, false},
		{"management listener", "management", map[string]bool{"10.0.0.250": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := inventoryFixture()
			listener := inventoryListener(v)
			profiles := map[string][4]any{
				"imaps":        {"imap", "0.0.0.0:1993", true, true},
				"smtp-inbound": {"smtp", "0.0.0.0:1025", true, false},
				"submission":   {"smtp", "0.0.0.0:1587", true, false},
				"submissions":  {"smtp", "0.0.0.0:1465", true, true},
				"management":   {"http", "0.0.0.0:8080", false, false},
			}
			p := profiles[tc.listener]
			listener["name"], listener["protocol"], listener["bind"] = tc.listener, p[0], map[string]bool{p[1].(string): true}
			listener["useTls"], listener["tlsImplicit"], listener["overrideProxyTrustedNetworks"] = p[2], p[3], tc.trust
			data, err := json.Marshal(withApplicationInventory(v))
			if err != nil {
				t.Fatal(err)
			}
			if got := validateInventory(data) == nil; got != tc.want {
				t.Fatalf("accepted=%v want=%v", got, tc.want)
			}
		})
	}
}

// The readback must refuse a listener whose stored PROXY trust differs from the
// plan in either direction: a stale trust the plan no longer grants, or an
// extra trusted address beside the configured one.
func TestReadbackComparesListenerProxyTrustExactly(t *testing.T) {
	listener := func(trust map[string]any) map[string]any {
		return map[string]any{"name": "imaps", "protocol": "imap", "overrideProxyTrustedNetworks": trust}
	}
	for _, tc := range []struct {
		name         string
		want, stored map[string]any
		match        bool
	}{
		{"both empty", map[string]any{}, map[string]any{}, true},
		{"same address", map[string]any{"10.0.0.250": true}, map[string]any{"10.0.0.250": true}, true},
		{"stale trust without an address", map[string]any{}, map[string]any{"10.0.0.250": true}, false},
		{"extra trusted address", map[string]any{"10.0.0.250": true}, map[string]any{"10.0.0.250": true, "10.0.0.251": true}, false},
		{"other address", map[string]any{"10.0.0.250": true}, map[string]any{"10.0.0.251": true}, false},
		{"loopback adapter extra peer", map[string]any{"127.0.0.1": true}, map[string]any{"127.0.0.1": true, "127.0.0.2": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyMatches(listener(tc.want), listener(tc.stored), ""); got != tc.match {
				t.Fatalf("readback match=%v want %v", got, tc.match)
			}
		})
	}
}
