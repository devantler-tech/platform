package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyIdentityUsesOnlyDedicatedLoopbackListenerAndRollsBack(t *testing.T) {
	for _, enabled := range []bool{false, true, false} {
		dir := bootstrapInputs(t)
		path := filepath.Join(dir, "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatal(err)
		}
		settings["useProxyClientIP"] = enabled
		data, err = json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		v, err := loadInputs(dir)
		if err != nil {
			t.Fatal("dedicated proxy identity setting refused")
		}
		plan, err := render(v)
		if err != nil {
			t.Fatal(err)
		}
		var seen bool
		for _, line := range strings.Split(strings.TrimSpace(string(plan)), "\n") {
			var op operation
			if err := json.Unmarshal([]byte(line), &op); err != nil {
				t.Fatal(err)
			}
			if op.Object == "Http" && op.Value["useXForwarded"] != false {
				t.Fatal("TCP identity must not also trust HTTP identity headers")
			}
			if op.Object == "SystemSettings" {
				global, ok := op.Value["proxyTrustedNetworks"].(map[string]any)
				if !ok || len(global) != 0 {
					t.Fatal("global proxy exemptions must stay explicitly empty, including rollback")
				}
			}
			if op.Object != "NetworkListener" {
				continue
			}
			listener, ok := op.Value["client-proxy"].(map[string]any)
			if !ok {
				t.Fatal("missing dedicated proxy listener")
			}
			bind, ok := listener["bind"].(map[string]any)
			if !ok || len(bind) != 1 || bind["127.0.0.1:18081"] != true || listener["protocol"] != "http" {
				t.Fatal("proxy listener must remain loopback-only")
			}
			trusted, ok := listener["overrideProxyTrustedNetworks"].(map[string]any)
			if !ok || (enabled && (len(trusted) != 1 || trusted["127.0.0.1"] != true)) || (!enabled && len(trusted) != 0) {
				t.Fatal("proxy trust must be explicit, local and reversible")
			}
			seen = true
		}
		if !seen {
			t.Fatal("missing dedicated loopback listener operation")
		}
	}
}

func TestProxyIdentityRefusesAmbiguousTransportSettings(t *testing.T) {
	for _, extra := range []string{`"useProxyClientIP":null`, `"useProxyClientIP":"true"`, `"useProxyClientIP":1`, `"useProxyClientIP":true,"useProxyClientIP":false`, `"useProxyClientIP":true,"useForwardedClientIP":true`} {
		dir := bootstrapInputs(t)
		path := filepath.Join(dir, "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.TrimSuffix(string(data), "}") + "," + extra + "}")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInputs(dir); err == nil {
			t.Fatal("ambiguous transport settings accepted")
		}
	}
}

func TestProxyListenerInventoryAcceptsOnlyItsManagedLoopbackTrust(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trust map[string]bool
		bind  string
		want  bool
	}{
		{"rollback", map[string]bool{}, "127.0.0.1:18081", true},
		{"enabled", map[string]bool{"127.0.0.1": true}, "127.0.0.1:18081", true},
		{"canonical prefix", map[string]bool{"127.0.0.1/32": true}, "127.0.0.1:18081", true},
		{"wildcard trust", map[string]bool{"0.0.0.0/0": true}, "127.0.0.1:18081", false},
		{"false trust entry", map[string]bool{"127.0.0.1": false}, "127.0.0.1:18081", false},
		{"extra trusted peer", map[string]bool{"127.0.0.1": true, "127.0.0.2": true}, "127.0.0.1:18081", false},
		{"public bind", map[string]bool{"127.0.0.1": true}, "0.0.0.0:18081", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := inventoryFixture()
			listener := inventoryListener(v)
			listener["name"], listener["protocol"], listener["bind"] = "client-proxy", "http", map[string]bool{tc.bind: true}
			listener["useTls"], listener["tlsImplicit"], listener["overrideProxyTrustedNetworks"] = false, false, tc.trust
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
