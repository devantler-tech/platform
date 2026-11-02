package main

import (
	"encoding/json"
	"testing"
)

func TestObservedTransportBindsIdentityWhileKeepingIntentSeparate(t *testing.T) {
	for _, tc := range []struct {
		annotation, status        string
		valid, proxy, statusProxy bool
	}{
		{"false", "VIP", true, false, false},
		{"true", "Proxy", true, true, true},
		{"false", "Proxy", true, false, true},
		{"true", "VIP", true, true, false},
		{"", "VIP", false, false, false},
		{"TRUE", "Proxy", false, false, false},
		{"false", "", false, false, false},
	} {
		t.Run(tc.annotation+tc.status, func(t *testing.T) {
			s := serviceFixture()
			s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = tc.annotation
			for _, item := range s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any) {
				item.(map[string]any)["ipMode"] = tc.status
			}
			raw, _ := json.Marshal(s)
			b, intent, err := deriveObserved(raw, gatewayFixture())
			if (err == nil) != tc.valid {
				t.Fatal("observed transport could not separate a known transition from unbound state")
			}
			if tc.valid && (intent.Proxy != tc.proxy || intent.StatusProxy != tc.statusProxy || b.HTTPSNodePort != 31443 || b.CIDR != "10.11.12.13/32") {
				t.Fatal("transport intent changed bound identity or conflated intent and observed status")
			}
			if tc.proxy {
				if _, err := derive(raw, gatewayFixture()); err == nil {
					t.Fatal("binding producer accepted an active transport")
				}
			}
		})
	}
}
