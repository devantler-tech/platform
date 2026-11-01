package main

import (
	"encoding/json"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"testing"
)

func disabledFixture() (map[string]any, binding) {
	b := binding{CIDR: "192.0.2.5/32", GatewayUID: "11111111-1111-4111-8111-111111111111"}
	m := map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumEnvoyConfig", "metadata": map[string]any{"name": "cilium-gateway-platform", "namespace": "kube-system", "uid": "22222222-2222-4222-8222-222222222222", "resourceVersion": "100", "annotations": map[string]any{"platform.devantler.tech/gateway-receiver-owner": "v1", "example.invalid/keep": "yes"}, "ownerReferences": []any{map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "name": "platform", "uid": b.GatewayUID, "controller": true}}}, "spec": map[string]any{"resources": []any{map[string]any{"@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster", "name": "preserve"}, map[string]any{"@type": "type.googleapis.com/envoy.config.listener.v3.Listener", "name": "listener", "filterChains": []any{map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "raw_buffer"}}, map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "tls", "serverNames": []any{"example.invalid"}, "directSourcePrefixRanges": []any{map[string]any{"addressPrefix": "192.0.2.5", "prefixLen": 32}}}}}}}}}
	return m, b
}
func TestDisabledReceiverRestoresOnlyItsOwnedFieldsWithExactObjectPreconditions(t *testing.T) {
	m, b := disabledFixture()
	raw, _ := json.Marshal(m)
	p, err := disabledPatch(raw, b)
	if err != nil || len(p) == 0 {
		t.Fatal("owned receiver state was not restored")
	}
	patch, err := jsonpatch.DecodePatch(p)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := patch.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if json.Unmarshal(restored, &got) != nil {
		t.Fatal("patched object")
	}
	meta := got["metadata"].(map[string]any)
	anns := meta["annotations"].(map[string]any)
	if anns["example.invalid/keep"] != "yes" || anns["platform.devantler.tech/gateway-receiver-owner"] != nil {
		t.Fatal("foreign metadata removed or ownership retained")
	}
	resources := got["spec"].(map[string]any)["resources"].([]any)
	chains := resources[1].(map[string]any)["filterChains"].([]any)
	match := chains[1].(map[string]any)["filterChainMatch"].(map[string]any)
	if match["directSourcePrefixRanges"] != nil || match["serverNames"].([]any)[0] != "example.invalid" || resources[0].(map[string]any)["name"] != "preserve" {
		t.Fatal("unowned route or resource changed")
	}
	for _, key := range []string{"uid", "resourceVersion"} {
		changed := map[string]any{}
		_ = json.Unmarshal(raw, &changed)
		changed["metadata"].(map[string]any)[key] = "replacement"
		other, _ := json.Marshal(changed)
		if _, err := patch.Apply(other); err == nil {
			t.Fatal("patch crossed object replacement or concurrent update")
		}
	}
	p, err = disabledPatch(restored, b)
	if err != nil || len(p) != 0 {
		t.Fatal("disabled state was not idempotent")
	}
}
func TestDisabledReceiverRefusesForeignOrAmbiguousState(t *testing.T) {
	for _, bad := range []string{"marker", "peer", "prefix", "gateway", "deleting", "fallback", "matcher", "proxy-listener"} {
		t.Run(bad, func(t *testing.T) {
			m, b := disabledFixture()
			meta := m["metadata"].(map[string]any)
			l := m["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
			match := l["filterChains"].([]any)[1].(map[string]any)["filterChainMatch"].(map[string]any)
			switch bad {
			case "marker":
				delete(meta["annotations"].(map[string]any), "platform.devantler.tech/gateway-receiver-owner")
			case "peer":
				match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["addressPrefix"] = "192.0.2.6"
			case "prefix":
				match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["prefixLen"] = 24
			case "gateway":
				meta["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "deleting":
				meta["deletionTimestamp"] = "2026-10-29T00:00:00Z"
			case "fallback":
				l["defaultFilterChain"] = map[string]any{}
			case "matcher":
				l["filterChainMatcher"] = map[string]any{}
			case "proxy-listener":
				l["listenerFilters"] = []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol"}}
			}
			raw, _ := json.Marshal(m)
			if _, err := disabledPatch(raw, b); err == nil {
				t.Fatal("unknown or foreign state was modified")
			}
		})
	}
}
