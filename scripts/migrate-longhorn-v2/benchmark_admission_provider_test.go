package main

import (
	"encoding/json"
	"net/netip"
	"testing"
)

func TestBenchmarkDirectEndpointRequiresMatchingProviderServer(t *testing.T) {
	endpoint := benchmarkAPIServer{
		Node: "prod-control-plane-1", ProviderID: "hcloud://1001",
		InternalIP: netip.MustParseAddr("10.0.0.1"), ExternalIP: netip.MustParseAddr("203.0.113.1"),
	}
	for _, test := range []struct {
		name   string
		edit   func(object)
		proven bool
	}{
		{"exact running KSail server", func(object) {}, true},
		{"different provider ID", func(s object) { s["id"] = 1002 }, false},
		{"different server name", func(s object) { s["name"] = "other" }, false},
		{"not running", func(s object) { s["status"] = "off" }, false},
		{"unowned", func(s object) { delete(valueObject(s, "labels"), "ksail.owned") }, false},
		{"different cluster", func(s object) { valueObject(s, "labels")["ksail.cluster.name"] = "dev" }, false},
		{"different role", func(s object) { valueObject(s, "labels")["ksail.node.type"] = "worker" }, false},
		{"different ordinal", func(s object) { valueObject(s, "labels")["ksail.node.index"] = "2" }, false},
		{"blocked public address", func(s object) { valueObject(s, "public_net", "ipv4")["blocked"] = true }, false},
		{"different public IPv4", func(s object) { valueObject(s, "public_net", "ipv4")["ip"] = "203.0.113.2" }, false},
		{"different private IPv4", func(s object) { s["private_net"] = []object{{"ip": "10.0.0.2"}} }, false},
		{"multiple provider private networks", func(s object) {
			s["private_net"] = []object{{"ip": "10.0.0.1"}, {"ip": "10.0.0.1"}}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := object{
				"id": 1001, "name": endpoint.Node, "status": "running",
				"labels":      object{"ksail.owned": "true", "ksail.cluster.name": "prod", "ksail.node.type": "control-plane", "ksail.node.index": "1"},
				"public_net":  object{"ipv4": object{"ip": "203.0.113.1"}},
				"private_net": []object{{"ip": "10.0.0.1"}},
			}
			test.edit(server)
			body, err := json.Marshal(object{"server": server})
			if err != nil {
				t.Fatal(err)
			}
			if got := benchmarkProviderServerMatches(body, endpoint); got != test.proven {
				t.Fatalf("provider endpoint proven = %t, want %t", got, test.proven)
			}
		})
	}
}
