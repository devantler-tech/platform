package main

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRecoveryBindingHandlesStaleTransportStatusWithoutRelaxingOwnership(t *testing.T) {
	for _, bad := range []string{"valid", "mixed", "missing", "enabled-desired", "foreign-owner"} {
		t.Run(bad, func(t *testing.T) {
			s := serviceFixture()
			ingress := s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any)
			for _, item := range ingress {
				item.(map[string]any)["ipMode"] = "Proxy"
			}
			switch bad {
			case "mixed":
				ingress[0].(map[string]any)["ipMode"] = "VIP"
			case "missing":
				delete(ingress[0].(map[string]any), "ipMode")
			case "enabled-desired":
				s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = "true"
			case "foreign-owner":
				s["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			}
			raw, _ := json.Marshal(s)
			if _, err := derive(raw, gatewayFixture()); err == nil {
				t.Fatal("ordinary baseline admission accepted transitional status")
			}
			got, err := deriveRecovery(raw, gatewayFixture())
			if (err == nil) != (bad == "valid") {
				t.Fatal("recovery could not distinguish transitional status from unbound state")
			}
			if err == nil && (got.CIDR != "10.11.12.13/32" || got.HTTPSNodePort != 31443) {
				t.Fatal("recovery lost the bound source or destination")
			}
		})
	}
}

func TestProviderModeDisagreementIsDistinctFromUnverifiableIdentity(t *testing.T) {
	for _, bad := range []string{"mode-only", "bad-destination", "bad-target"} {
		t.Run(bad, func(t *testing.T) {
			lb, b := providerFixture()
			for _, item := range lb["services"].([]any) {
				item.(map[string]any)["proxyprotocol"] = true
			}
			if bad == "bad-destination" {
				lb["services"].([]any)[1].(map[string]any)["destination_port"] = 443
			}
			if bad == "bad-target" {
				lb["targets"] = []any{}
			}
			raw, _ := json.Marshal(lb)
			var actual providerBalancer
			if providerJSON(raw, &actual) != nil {
				t.Fatal("provider fixture")
			}
			_, err := providerMatches(actual, b, false)
			if err == nil || errors.Is(err, errProviderMode) != (bad == "mode-only") {
				t.Fatal("unknown provider identity was treated as a settling mode")
			}
		})
	}
}
