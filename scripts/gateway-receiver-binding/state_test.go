package main

import (
	"encoding/json"
	"testing"
)

func activeReceiverFilters() []any {
	return []any{
		map[string]any{"name": "envoy.filters.listener.proxy_protocol", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol"}},
		map[string]any{"name": "envoy.filters.listener.tls_inspector", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector"}},
	}
}

func TestReceiverInspectionSeparatesKnownModesWithoutAuthorizingActiveCleanup(t *testing.T) {
	for _, mode := range []receiverMode{receiverDisabled, receiverFencedDisabled, receiverFencedActive} {
		t.Run(string(mode), func(t *testing.T) {
			m, b := disabledFixture()
			listener := m["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
			if mode == receiverDisabled {
				delete(m["metadata"].(map[string]any)["annotations"].(map[string]any), receiverOwner)
				delete(listener["filterChains"].([]any)[1].(map[string]any)["filterChainMatch"].(map[string]any), "directSourcePrefixRanges")
			}
			if mode == receiverFencedActive {
				listener["listenerFilters"] = activeReceiverFilters()
			}
			raw, _ := json.Marshal(m)
			got, err := inspectReceiver(raw, b)
			if err != nil || got.Mode != mode || got.UID != "22222222-2222-4222-8222-222222222222" || got.Version != "100" {
				t.Fatal("known receiver mode or exact object identity was not observed")
			}
			if mode == receiverFencedActive {
				if _, err = disabledPatch(raw, b); err == nil {
					t.Fatal("inspection weakened active receiver cleanup refusal")
				}
			}
		})
	}
}

func TestReceiverInspectionRefusesPartialOrUnknownActiveState(t *testing.T) {
	for _, bad := range []string{"missing-peer", "mixed-peers", "wrong-peer", "missing-marker", "orphan-marker", "optional-proxy", "extra-proxy-field", "unknown-filter", "filter-order", "proxy-alias", "filter-alias", "foreign-owner", "fallback"} {
		t.Run(bad, func(t *testing.T) {
			m, b := disabledFixture()
			listener := m["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
			filters := activeReceiverFilters()
			listener["listenerFilters"] = filters
			match := listener["filterChains"].([]any)[1].(map[string]any)["filterChainMatch"].(map[string]any)
			switch bad {
			case "missing-peer":
				delete(match, "directSourcePrefixRanges")
			case "mixed-peers":
				listener["filterChains"] = append(listener["filterChains"].([]any), map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "tls"}})
			case "wrong-peer":
				match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["addressPrefix"] = "192.0.2.6"
			case "missing-marker":
				delete(m["metadata"].(map[string]any)["annotations"].(map[string]any), receiverOwner)
			case "orphan-marker":
				delete(match, "directSourcePrefixRanges")
				delete(listener, "listenerFilters")
			case "optional-proxy":
				filters[0].(map[string]any)["typedConfig"].(map[string]any)["allowRequestsWithoutProxyProtocol"] = true
			case "extra-proxy-field":
				filters[0].(map[string]any)["typedConfig"].(map[string]any)["rules"] = []any{}
			case "unknown-filter":
				filters[1].(map[string]any)["name"] = "other"
			case "filter-order":
				listener["listenerFilters"] = []any{filters[1], filters[0]}
			case "proxy-alias":
				filters[0].(map[string]any)["typed_config"] = filters[0].(map[string]any)["typedConfig"]
				delete(filters[0].(map[string]any), "typedConfig")
			case "filter-alias":
				listener["listener_filters"] = filters
				delete(listener, "listenerFilters")
			case "foreign-owner":
				m["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "fallback":
				listener["defaultFilterChain"] = map[string]any{}
			}
			raw, _ := json.Marshal(m)
			if _, err := inspectReceiver(raw, b); err == nil {
				t.Fatal("unknown or partial receiver state was classified as safe")
			}
		})
	}
}
