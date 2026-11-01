package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func inventoryFixture() map[string]any {
	return map[string]any{"methodResponses": []any{
		[]any{"x:NetworkListener/query", map[string]any{"ids": []string{"one"}, "total": 1, "position": 0}, "listeners-query"},
		[]any{"x:NetworkListener/get", map[string]any{"list": []any{map[string]any{"id": "one", "name": "imaps", "protocol": "imap", "bind": map[string]bool{"0.0.0.0:1993": true}, "useTls": true, "tlsImplicit": true, "overrideProxyTrustedNetworks": map[string]bool{}, "tlsDisableProtocols": map[string]bool{}}}, "notFound": []string{}}, "listeners-get"},
		[]any{"x:MtaHook/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "MtaHook"},
		[]any{"x:MtaMilter/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "MtaMilter"},
		[]any{"x:WebHook/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "WebHook"},
		[]any{"x:Domain/query", map[string]any{"ids": []string{"domain"}, "total": 1, "position": 0}, "domains-query"},
		[]any{"x:Domain/get", map[string]any{"list": []any{map[string]any{"id": "domain", "name": "example.test", "aliases": map[string]bool{}, "directoryId": nil}}, "notFound": []string{}}, "domains-get"},
	}}
}
func TestInventoryRefusesIncompleteOrUnexpectedServingProfiles(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["methodResponses"] = []any{} },
		func(v map[string]any) { inventoryDomain(v)["name"] = "example.invalid" },
		func(v map[string]any) { inventoryDomain(v)["aliases"] = map[string]bool{"EXAMPLE.INVALID": true} },
		func(v map[string]any) { inventoryDomain(v)["directoryId"] = "unexpected" },
		func(v map[string]any) { delete(inventoryDomain(v), "aliases") },
		func(v map[string]any) { v["methodResponses"].([]any)[5].([]any)[1].(map[string]any)["total"] = 2 },
		func(v map[string]any) { v["methodResponses"].([]any)[2].([]any)[1].(map[string]any)["total"] = 1 },
		func(v map[string]any) { v["methodResponses"].([]any)[0].([]any)[1].(map[string]any)["total"] = 2 },
		func(v map[string]any) { delete(v["methodResponses"].([]any)[0].([]any)[1].(map[string]any), "total") },
		func(v map[string]any) {
			v["methodResponses"].([]any)[1].([]any)[1].(map[string]any)["notFound"] = []string{"one"}
		},
		func(v map[string]any) {
			inventoryListener(v)["bind"] = map[string]bool{"0.0.0.0:1993": true, "[::]:993": true}
		},
		func(v map[string]any) { inventoryListener(v)["useTls"] = false },
		func(v map[string]any) { delete(inventoryListener(v), "useTls") },
		func(v map[string]any) { inventoryListener(v)["name"] = "unexpected" },
		func(v map[string]any) { inventoryListener(v)["id"] = "wrong" },
		func(v map[string]any) {
			inventoryListener(v)["overrideProxyTrustedNetworks"] = map[string]bool{"0.0.0.0/0": true}
		},
	} {
		v := inventoryFixture()
		change(v)
		b, err := json.Marshal(withApplicationInventory(v))
		if err != nil {
			t.Fatal(err)
		}
		if validateInventory(b) == nil {
			t.Fatal("unsafe inventory accepted")
		}
	}
	b, err := json.Marshal(withApplicationInventory(inventoryFixture()))
	if err != nil || validateInventory(b) != nil {
		t.Fatal("exact recognized partial recovery inventory refused")
	}
	if validateInventory(emptyInventory(t)) != nil {
		t.Fatal("verified fresh recovery inventory refused")
	}
}

func emptyInventory(t *testing.T) []byte {
	t.Helper()
	v := receivingInventory(nil, nil)
	methods := v["methodResponses"].([]any)
	methods[0].([]any)[1] = map[string]any{"ids": []string{}, "total": 0, "position": 0}
	methods[1].([]any)[1] = map[string]any{"list": []any{}, "notFound": []string{}}
	methods[5].([]any)[1] = map[string]any{"ids": []string{}, "total": 0, "position": 0}
	methods[6].([]any)[1] = map[string]any{"list": []any{}, "notFound": []string{}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func inventoryListener(v map[string]any) map[string]any {
	return v["methodResponses"].([]any)[1].([]any)[1].(map[string]any)["list"].([]any)[0].(map[string]any)
}

func inventoryDomain(v map[string]any) map[string]any {
	return v["methodResponses"].([]any)[6].([]any)[1].(map[string]any)["list"].([]any)[0].(map[string]any)
}

func withApplicationInventory(v map[string]any, applications ...map[string]any) map[string]any {
	ids, list := []string{}, []any{}
	for _, application := range applications {
		ids = append(ids, application["id"].(string))
		list = append(list, application)
	}
	v["methodResponses"] = append(v["methodResponses"].([]any),
		[]any{"x:Application/query", map[string]any{"ids": ids, "total": len(ids), "position": 0}, "applications-query"},
		[]any{"x:Application/get", map[string]any{"list": list, "notFound": []string{}}, "applications-get"})
	return v
}

func applicationInventoryFixture() map[string]any {
	return map[string]any{"id": "webadmin", "description": "Stalwart Web Interface", "enabled": true,
		"resourceUrl": "https://github.com/stalwartlabs/webui/releases/download/v1.0.11/webui.zip",
		"urlPrefix":   map[string]bool{"/admin": true, "/account": true}, "unpackDirectory": nil, "oauthClientId": nil}
}

func TestInventoryRequiresCompleteAndRecognizedApplications(t *testing.T) {
	for _, resource := range []string{
		"https://github.com/stalwartlabs/webui/releases/download/v1.0.11/webui.zip",
		"file:///usr/local/share/stalwart/webui.zip",
		expectedAdministrationURI(t),
		"file:///usr/local/share/stalwart/webui-" + strings.Repeat("a", 64) + ".zip",
	} {
		application := applicationInventoryFixture()
		application["resourceUrl"] = resource
		raw, err := json.Marshal(withApplicationInventory(inventoryFixture(), application))
		if err != nil || validateInventory(raw) != nil {
			t.Fatal("recognized administration application inventory refused")
		}
	}
	raw, err := json.Marshal(withApplicationInventory(inventoryFixture()))
	if err != nil || validateInventory(raw) != nil {
		t.Fatal("verified empty application inventory refused")
	}
}

func TestInventoryRefusesAnOmittedApplicationCensus(t *testing.T) {
	raw, err := json.Marshal(inventoryFixture())
	if err != nil {
		t.Fatal(err)
	}
	if validateInventory(raw) == nil {
		t.Fatal("listener inventory alone cannot prove the application serving profile")
	}
}

func TestInventoryRefusesUnexpectedOrIncompleteApplications(t *testing.T) {
	for name, change := range map[string]func(map[string]any, map[string]any){
		"external asset": func(_ map[string]any, a map[string]any) {
			a["resourceUrl"] = "https://unexpected.example.test/webui.zip"
		},
		"another local file": func(_ map[string]any, a map[string]any) { a["resourceUrl"] = "file:///tmp/unverified.zip" },
		"non-digest file": func(_ map[string]any, a map[string]any) {
			a["resourceUrl"] = "file:///usr/local/share/stalwart/webui-version.zip"
		},
		"digest path traversal": func(_ map[string]any, a map[string]any) {
			a["resourceUrl"] = "file:///usr/local/share/stalwart/webui-" + strings.Repeat("a", 64) + ".zip/../private"
		},
		"another application":  func(_ map[string]any, a map[string]any) { a["description"] = "unexpected" },
		"disabled application": func(_ map[string]any, a map[string]any) { a["enabled"] = false },
		"missing enablement":   func(_ map[string]any, a map[string]any) { delete(a, "enabled") },
		"extra route": func(_ map[string]any, a map[string]any) {
			a["urlPrefix"] = map[string]bool{"/admin": true, "/account": true, "/": true}
		},
		"missing route":            func(_ map[string]any, a map[string]any) { a["urlPrefix"] = map[string]bool{"/admin": true} },
		"other unpack directory":   func(_ map[string]any, a map[string]any) { a["unpackDirectory"] = "/tmp/unverified" },
		"missing unpack directory": func(_ map[string]any, a map[string]any) { delete(a, "unpackDirectory") },
		"external OAuth client":    func(_ map[string]any, a map[string]any) { a["oauthClientId"] = "external" },
		"missing OAuth state":      func(_ map[string]any, a map[string]any) { delete(a, "oauthClientId") },
		"wrong identity":           func(_ map[string]any, a map[string]any) { a["id"] = "other" },
		"missing total": func(v map[string]any, _ map[string]any) {
			delete(v["methodResponses"].([]any)[7].([]any)[1].(map[string]any), "total")
		},
		"truncated query": func(v map[string]any, _ map[string]any) {
			v["methodResponses"].([]any)[7].([]any)[1].(map[string]any)["total"] = 2
		},
		"wrong position": func(v map[string]any, _ map[string]any) {
			v["methodResponses"].([]any)[7].([]any)[1].(map[string]any)["position"] = 1
		},
		"missing get result": func(v map[string]any, _ map[string]any) {
			v["methodResponses"].([]any)[8].([]any)[1].(map[string]any)["notFound"] = []string{"webadmin"}
		},
		"multiple applications": func(v map[string]any, a map[string]any) {
			q := v["methodResponses"].([]any)[7].([]any)[1].(map[string]any)
			q["ids"], q["total"] = []string{"webadmin", "other"}, 2
			other := applicationInventoryFixture()
			other["id"] = "other"
			v["methodResponses"].([]any)[8].([]any)[1].(map[string]any)["list"] = []any{a, other}
		},
	} {
		t.Run(name, func(t *testing.T) {
			application := applicationInventoryFixture()
			v := withApplicationInventory(inventoryFixture(), application)
			change(v, application)
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if validateInventory(raw) == nil {
				t.Fatal("unexpected or incomplete application inventory accepted")
			}
		})
	}
}
