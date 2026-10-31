package main

import (
	"encoding/json"
	"testing"
)

func gatewayFixture() []byte {
	return []byte(`{"metadata":{"name":"platform","namespace":"kube-system","uid":"11111111-1111-4111-8111-111111111111","generation":2,"resourceVersion":"44"},"spec":{"gatewayClassName":"cilium"},"status":{"conditions":[{"type":"Accepted","status":"True","observedGeneration":2},{"type":"Programmed","status":"True","observedGeneration":2}]}}`)
}
func serviceFixture() map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "cilium-gateway-platform", "namespace": "kube-system", "uid": "22222222-2222-4222-8222-222222222222", "resourceVersion": "55", "labels": map[string]any{"io.cilium.gateway/owning-gateway": "platform"}, "annotations": map[string]any{"load-balancer.hetzner.cloud/name": "platform-gateway", "load-balancer.hetzner.cloud/use-private-ip": "true", "load-balancer.hetzner.cloud/uses-proxyprotocol": "false"}, "ownerReferences": []any{map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "name": "platform", "uid": "11111111-1111-4111-8111-111111111111", "controller": true}}}, "spec": map[string]any{"type": "LoadBalancer", "externalTrafficPolicy": "Cluster", "ipFamilies": []any{"IPv4"}, "ports": []any{map[string]any{"port": 80, "protocol": "TCP", "nodePort": 31080}, map[string]any{"port": 443, "protocol": "TCP", "nodePort": 31443}}}, "status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "192.0.2.1", "ipMode": "VIP"}, map[string]any{"ip": "10.11.12.13", "ipMode": "VIP"}}}}}
}
func TestProviderBindingRequiresOnePrivateHostAndItsGatewayOwner(t *testing.T) {
	good, _ := json.Marshal(serviceFixture())
	b, err := derive(good, gatewayFixture())
	if err != nil || b.CIDR != "10.11.12.13/32" || b.ServiceUID != "22222222-2222-4222-8222-222222222222" {
		t.Fatal("complete provider binding refused")
	}
	tests := map[string]func(map[string]any){
		"wrong-service": func(s map[string]any) { s["metadata"].(map[string]any)["name"] = "other" },
		"unowned":       func(s map[string]any) { s["metadata"].(map[string]any)["ownerReferences"] = []any{} },
		"different-gateway": func(s map[string]any) {
			s["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
		},
		"wrong-provider": func(s map[string]any) {
			s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/name"] = "other"
		},
		"wrong-path": func(s map[string]any) {
			s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/use-private-ip"] = "false"
		},
		"enabled-baseline": func(s map[string]any) {
			s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = "true"
		},
		"missing-private": func(s map[string]any) {
			s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"] = []any{map[string]any{"ip": "192.0.2.1", "ipMode": "VIP"}}
		},
		"ambiguous-private": func(s map[string]any) {
			lb := s["status"].(map[string]any)["loadBalancer"].(map[string]any)
			lb["ingress"] = append(lb["ingress"].([]any), map[string]any{"ip": "10.11.12.14", "ipMode": "VIP"})
		},
		"prefix-not-host": func(s map[string]any) {
			s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any)[1].(map[string]any)["ip"] = "10.11.12.0/24"
		},
		"extra-listener": func(s map[string]any) {
			spec := s["spec"].(map[string]any)
			spec["ports"] = append(spec["ports"].([]any), map[string]any{"port": 25, "protocol": "TCP", "nodePort": 31025})
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s := serviceFixture()
			change(s)
			raw, _ := json.Marshal(s)
			if _, err := derive(raw, gatewayFixture()); err == nil {
				t.Fatal("unbound provider address accepted")
			}
		})
	}
}
func TestGatewayBindingRefusesUnobservedControllerState(t *testing.T) {
	raw, _ := json.Marshal(serviceFixture())
	var g map[string]any
	_ = json.Unmarshal(gatewayFixture(), &g)
	g["metadata"].(map[string]any)["generation"] = 3
	changed, _ := json.Marshal(g)
	if _, err := derive(raw, changed); err == nil {
		t.Fatal("stale gateway status accepted")
	}
}
