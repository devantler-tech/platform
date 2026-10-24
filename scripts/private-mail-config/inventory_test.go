package main

import (
	"encoding/json"
	"testing"
)

func inventoryFixture() map[string]any {
	return map[string]any{"methodResponses": []any{
		[]any{"x:NetworkListener/query", map[string]any{"ids": []string{"one"}, "total": 1, "position": 0}, "listeners-query"},
		[]any{"x:NetworkListener/get", map[string]any{"list": []any{map[string]any{"id": "one", "name": "imaps", "protocol": "imap", "bind": map[string]bool{"0.0.0.0:1993": true}, "useTls": true, "tlsImplicit": true, "overrideProxyTrustedNetworks": map[string]bool{}, "tlsDisableProtocols": map[string]bool{}}}, "notFound": []string{}}, "listeners-get"},
		[]any{"x:MtaHook/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "MtaHook"},
		[]any{"x:MtaMilter/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "MtaMilter"},
		[]any{"x:WebHook/query", map[string]any{"ids": []string{}, "total": 0, "position": 0}, "WebHook"},
	}}
}
func TestInventoryRefusesIncompleteOrUnexpectedServingProfiles(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["methodResponses"] = []any{} },
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
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if validateInventory(b) == nil {
			t.Fatal("unsafe inventory accepted")
		}
	}
	b, err := json.Marshal(inventoryFixture())
	if err != nil || validateInventory(b) != nil {
		t.Fatal("exact recognized partial recovery inventory refused")
	}
	if validateInventory(emptyInventory(t)) != nil {
		t.Fatal("verified fresh recovery inventory refused")
	}
}

func emptyInventory(t *testing.T) []byte {
	t.Helper()
	v := inventoryFixture()
	methods := v["methodResponses"].([]any)
	methods[0].([]any)[1] = map[string]any{"ids": []string{}, "total": 0, "position": 0}
	methods[1].([]any)[1] = map[string]any{"list": []any{}, "notFound": []string{}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func inventoryListener(v map[string]any) map[string]any {
	return v["methodResponses"].([]any)[1].([]any)[1].(map[string]any)["list"].([]any)[0].(map[string]any)
}
