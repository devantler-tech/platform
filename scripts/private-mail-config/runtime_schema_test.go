package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelectedRuntimeEmitsItsNativeSchema(t *testing.T) {
	for _, version := range []string{"0.16.27", "1.0.2"} {
		t.Run(version, func(t *testing.T) {
			settings := strings.Replace(aliasesSettings, "{", `{"runtimeVersion":"`+version+`",`, 1)
			inventory := receivingInventory(nil, nil)
			if version == "1.0.2" {
				listener := inventoryListener(inventory)
				listener["implicitTls"] = listener["tlsImplicit"]
				delete(listener, "tlsImplicit")
			}
			input, output := aliasInputs(t, settings, inventory)
			var log bytes.Buffer
			if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
				t.Fatalf("reviewed runtime schema refused: %s", &log)
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := planOperations(raw)
			if err != nil {
				t.Fatal(err)
			}
			domainFlag, listenerFlag, appFlag, interval, aliasFlag := "isEnabled", "tlsImplicit", "enabled", "autoUpdateFrequency", "enabled"
			if version == "1.0.2" {
				domainFlag, listenerFlag, appFlag, interval, aliasFlag = "enable", "implicitTls", "enable", "autoUpdateInterval", "enable"
			}
			found := map[string]bool{}
			for _, op := range ops {
				switch op.Object {
				case "Domain":
					d := op.Value["domain"].(map[string]any)
					if d[domainFlag] != true || len(d) != 7 {
						t.Fatal("domain schema or relay policy changed")
					}
					found["domain"] = true
				case "NetworkListener":
					l := op.Value["imap"].(map[string]any)
					if l[listenerFlag] != true || l["useTls"] != true || len(l) != 5 {
						t.Fatal("implicit TLS schema changed")
					}
					found["listener"] = true
				case "Application":
					a := op.Value["webadmin"].(map[string]any)
					if a[appFlag] != true || a[interval] != float64(2592000000) || len(a) != 7 {
						t.Fatal("application schema changed")
					}
					digest := "d67e68caba02024301c79da9f15fe465de9bf5382cee54bdf9c309a97b4179a5"
					if version == "1.0.2" {
						digest = "7278be67087463e011abf40178a2ff073d01bbf02e62ad2a3ff04435140022b0"
					}
					if a["resourceUrl"] != "file:///usr/local/share/stalwart/webui-"+digest+".zip" {
						t.Fatal("runtime and administration asset mismatch")
					}
					found["application"] = true
				case "Http":
					if version == "1.0.2" {
						if op.Value["clientIpSource"] != "peer" || op.Value["forwardedHops"] != float64(1) {
							t.Fatal("untrusted forwarded identity enabled")
						}
						if _, exists := op.Value["useXForwarded"]; exists {
							t.Fatal("legacy identity field emitted")
						}
					} else if op.Value["useXForwarded"] != false {
						t.Fatal("legacy identity policy changed")
					}
					found["http"] = true
				case "Account":
					if op.Type != "create" {
						continue
					}
					m := op.Value["maintainer"].(map[string]any)
					a := m["aliases"].(map[string]any)["0"].(map[string]any)
					if a[aliasFlag] != true || len(a) != 4 {
						t.Fatal("receiving alias schema changed")
					}
					p := m["permissions"].(map[string]any)["enabledPermissions"].(map[string]any)
					wantCount := 244
					if version == "1.0.2" {
						wantCount = 248
					}
					if len(p) != wantCount || p["authenticate"] != true || p["jmapEmailGet"] != true || p["sysAccountUpdate"] != nil {
						t.Fatal("mailbox grants broadened or incomplete")
					}
					found["account"] = true
				}
			}
			if len(found) != 5 {
				t.Fatal("runtime schema families not covered")
			}
		})
	}
}

func TestRuntimeVersionRefusesUnknownOrAmbiguousInputs(t *testing.T) {
	for _, member := range []string{`"runtimeVersion":""`, `"runtimeVersion":null`, `"runtimeVersion":1`, `"runtimeVersion":"1.0.3"`, `"runtimeVersion":"1.0.2","runtimeVersion":"0.16.27"`} {
		input, output := aliasInputs(t, strings.Replace(aliasesSettings, "{", "{"+member+",", 1), receivingInventory(nil, nil))
		if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
			t.Fatal("unsupported runtime created a plan")
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("refused runtime left an output")
		}
	}
}

func TestSchemaSelectionRefusesForeignOrMixedListenerFields(t *testing.T) {
	for _, version := range []string{"0.16.27", "1.0.2"} {
		for _, mixed := range []bool{false, true} {
			inventory := receivingInventory(nil, nil)
			l := inventoryListener(inventory)
			if version == "0.16.27" {
				l["implicitTls"] = true
				if !mixed {
					delete(l, "tlsImplicit")
				}
			} else if mixed {
				l["implicitTls"] = true
			}
			settings := strings.Replace(aliasesSettings, "{", `{"runtimeVersion":"`+version+`",`, 1)
			input, output := aliasInputs(t, settings, inventory)
			if command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{}) == 0 {
				t.Fatal("foreign or mixed native schema accepted")
			}
		}
	}
}

func TestSchemaSelectionRefusesForeignNullFields(t *testing.T) {
	for _, version := range []string{"0.16.27", "1.0.2"} {
		for _, family := range []string{"listener", "application"} {
			app := applicationInventoryFixture()
			inventory := withApplicationInventory(inventoryFixture(), app)
			if version == "1.0.2" {
				l := inventoryListener(inventory)
				l["implicitTls"] = true
				delete(l, "tlsImplicit")
				app["enable"] = true
				delete(app, "enabled")
				app["resourceUrl"] = "https://github.com/stalwartlabs/webui/releases/download/v2.0.4/webui.zip"
			}
			if family == "listener" {
				field := "implicitTls"
				if version == "1.0.2" {
					field = "tlsImplicit"
				}
				inventoryListener(inventory)[field] = nil
			} else {
				field := "enable"
				if version == "1.0.2" {
					field = "enabled"
				}
				app[field] = nil
			}
			raw, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			if validateInventoryFor(raw, version) == nil {
				t.Fatalf("%s %s accepted a foreign null field", version, family)
			}
		}
	}
}

func TestNewRuntimeChecksMatchedToolsBeforeOpeningTheDatastore(t *testing.T) {
	for _, tc := range []struct {
		server, cli string
		starts      bool
	}{
		{"1.0.2", "stalwart-cli 1.0.14", true},
		{"0.16.27", "stalwart-cli 1.0.14", false},
		{"1.0.2", "stalwart-cli 1.0.13", false},
		{"1.0.2 extra", "stalwart-cli 1.0.14", false},
	} {
		t.Run(tc.server+"/"+tc.cli, func(t *testing.T) {
			dir := bootstrapInputs(t)
			data := t.TempDir()
			started := filepath.Join(data, "started")
			server, cli := filepath.Join(data, "server"), filepath.Join(data, "cli")
			for path, body := range map[string]string{
				server: "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' '" + tc.server + "'; exit; fi\nprintf started > '" + started + "'\nexec /bin/sleep 30\n",
				cli:    "#!/bin/sh\nprintf '%s\\n' '" + tc.cli + "'\n",
			} {
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			settings, err := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			settings = bytes.Replace(settings, []byte("{"), []byte(`{"runtimeVersion":"1.0.2",`), 1)
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), settings, 0600); err != nil {
				t.Fatal(err)
			}
			asset, digest := applicationAssetFixture(t)
			r := bootstrapRuntime{server: server, cli: cli, dataDir: data, url: "http://127.0.0.1:1", startup: 200 * time.Millisecond, shutdown: time.Second, applicationFile: asset, applicationDigest: digest}
			if bootstrap(context.Background(), dir, r) == nil {
				t.Fatal("non-serving fixture unexpectedly bootstrapped")
			}
			_, err = os.Stat(started)
			if (err == nil) != tc.starts {
				t.Fatal("unmatched tools opened the datastore, or matched tools were refused")
			}
		})
	}
}

func TestNewRuntimeReceivingAliasesStayWithTheirOwner(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		alias := map[string]any{"name": "former-a", "domainId": "domain", "enable": true, "description": nil}
		name := "maintainer"
		if wrong {
			name = "someone"
		}
		account := receivingOwner("user", name, map[string]any{"0": alias})
		inventory := receivingInventory([]map[string]any{account}, nil)
		l := inventoryListener(inventory)
		l["implicitTls"] = l["tlsImplicit"]
		delete(l, "tlsImplicit")
		settings := strings.Replace(aliasesSettings, "{", `{"runtimeVersion":"1.0.2",`, 1)
		// The retained set must match completely; this fixture declares one alias.
		settings = strings.Replace(settings, `["former-a","former-b"]`, `["former-a"]`, 1)
		input, output := aliasInputs(t, settings, inventory)
		result := command([]string{"--input-dir", input, "--output-file", output}, &bytes.Buffer{})
		if (result == 0) == wrong {
			t.Fatal("native alias ownership did not fence the plan")
		}
	}
	want := map[string]any{"0": map[string]any{"name": "former-a", "domainId": "domain", "enable": true, "description": nil}}
	if !receivingAliasesMatch(want, want) {
		t.Fatal("native receiving alias readback refused")
	}
	for _, actual := range []map[string]any{
		{"0": map[string]any{"name": "former-a", "domainId": "domain", "enabled": true, "description": nil}},
		{"0": map[string]any{"name": "former-a", "domainId": "domain", "enable": false, "description": nil}},
		{"0": map[string]any{"name": "former-a", "domainId": "domain", "enable": true, "enabled": true, "description": nil}},
	} {
		if receivingAliasesMatch(want, actual) {
			t.Fatal("foreign, disabled or mixed receiving alias accepted")
		}
	}
}

func TestNativeInventoryAndClientIdentityFollowTheSelectedSchema(t *testing.T) {
	for _, version := range []string{"0.16.27", "1.0.2"} {
		request, err := inventoryRequestFor(version)
		if err != nil {
			t.Fatal(err)
		}
		implicit, enabled := `"tlsImplicit"`, `"enabled"`
		if version == "1.0.2" {
			implicit, enabled = `"implicitTls"`, `"enable"`
		}
		if !bytes.Contains(request, []byte(implicit)) || !bytes.Contains(request, []byte(enabled)) {
			t.Fatal("native inventory requests foreign properties")
		}
		for _, forwarded := range []bool{false, true} {
			v := fixture()
			v.RuntimeVersion, v.UseForwardedClientIP = version, forwarded
			plan, err := render(v)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := planOperations(plan)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, op := range ops {
				if op.Object != "Http" {
					continue
				}
				found = true
				if version == "1.0.2" {
					want := "peer"
					if forwarded {
						want = "xForwardedFor"
					}
					if op.Value["clientIpSource"] != want {
						t.Fatal("forwarded client identity changed")
					}
				} else if op.Value["useXForwarded"] != forwarded {
					t.Fatal("legacy client identity changed")
				}
			}
			if !found {
				t.Fatal("no HTTP policy")
			}
			v.UseProxyClientIP = true
			if _, err := render(v); (err == nil) == forwarded {
				t.Fatal("mutually exclusive identity sources changed")
			}
		}
	}
}

func TestNewRuntimeAcceptsItsNativeFactoryInterface(t *testing.T) {
	app := applicationInventoryFixture()
	app["enable"] = true
	delete(app, "enabled")
	app["resourceUrl"] = "https://github.com/stalwartlabs/webui/releases/download/v2/webui.zip"
	inventory := withApplicationInventory(inventoryFixture(), app)
	l := inventoryListener(inventory)
	l["implicitTls"] = true
	delete(l, "tlsImplicit")
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if validateInventoryFor(raw, "1.0.2") != nil {
		t.Fatal("native 1.0.2 factory interface refused")
	}
}
