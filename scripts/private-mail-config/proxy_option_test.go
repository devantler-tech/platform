package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyCLIOverrideKeepsPrivateSettingsIntact(t *testing.T) {
	for _, tc := range []struct {
		name, argument string
		stored, want   bool
		valid          bool
	}{
		{"default off", "", false, false, true},
		{"legacy enabled", "", true, true, true},
		{"declarative activation", "--proxy-client-ip=true", false, true, true},
		{"declarative rollback", "--proxy-client-ip=false", true, false, true},
		{"bare activation", "--proxy-client-ip", false, true, true},
		{"invalid option", "--proxy-client-ip=private-canary", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fixture()
			v.UseProxyClientIP = tc.stored
			raw, err := json.Marshal(v.settings)
			if err != nil {
				t.Fatal(err)
			}
			input, output := aliasInputs(t, string(raw), receivingInventory(nil, nil))
			args := []string{"--input-dir", input, "--output-file", output}
			if tc.argument != "" {
				args = append(args, tc.argument)
			}
			var log bytes.Buffer
			if (command(args, &log) == 0) != tc.valid {
				t.Fatal("command did not honor the declarative transport override")
			}
			if strings.Contains(log.String(), "private-canary") {
				t.Fatal("invalid option leaked into diagnostics")
			}
			after, err := os.ReadFile(filepath.Join(input, "settings.json"))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("transport override rewrote the private settings")
			}
			if !tc.valid {
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("invalid override left a plan")
				}
				return
			}
			plan, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := planOperations(plan)
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, op := range ops {
				if op.Object != "NetworkListener" {
					continue
				}
				listener, ok := op.Value["client-proxy"].(map[string]any)
				if !ok {
					t.Fatal("proxy listener missing")
				}
				trust, ok := listener["overrideProxyTrustedNetworks"].(map[string]any)
				if !ok || (len(trust) != 0) != tc.want {
					t.Fatal("persisted proxy policy differs from the selected override")
				}
				seen = true
			}
			if !seen {
				t.Fatal("proxy listener was not examined")
			}
		})
	}
}

func TestBootstrapProxyOptionRejectsInvalidValuesWithoutDisclosure(t *testing.T) {
	var log bytes.Buffer
	if bootstrapCommand([]string{"--proxy-client-ip=private-canary"}, &log) != 2 || !strings.Contains(log.String(), "invalid arguments") || strings.Contains(log.String(), "private-canary") {
		t.Fatal("bootstrap did not refuse an invalid transport option safely")
	}
}

func TestProxyCLIOverrideStillRefusesConflictingHTTPForwarding(t *testing.T) {
	v := fixture()
	v.UseForwardedClientIP = true
	raw, err := json.Marshal(v.settings)
	if err != nil {
		t.Fatal(err)
	}
	input, output := aliasInputs(t, string(raw), receivingInventory(nil, nil))
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output, "--proxy-client-ip=true"}, &log) == 0 {
		t.Fatal("deployment override combined incompatible identity transports")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("conflicting transports left an output plan")
	}
}
