package preparation_test

import "testing"

func TestClientIdentityBundleReplacesUntrustedForwardingHeaders(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-client-identity")
	route := object(t, objects, "HTTPRoute", "private-mail")
	rules := sequence(t, mapping(t, route["spec"])["rules"])
	if len(rules) != 1 {
		t.Fatal("client identity must cover the entire administration route")
	}
	rule := mapping(t, rules[0])
	filters := sequence(t, rule["filters"])
	if len(filters) != 2 {
		t.Fatal("expected the existing response policy and one client identity policy")
	}
	response := mapping(t, filters[0])
	if response["type"] != "ResponseHeaderModifier" {
		t.Fatal("client identity must preserve the existing response security policy")
	}
	responseSet := sequence(t, mapping(t, response["responseHeaderModifier"])["set"])
	if len(responseSet) != 1 || mapping(t, responseSet[0])["name"] != "Strict-Transport-Security" || mapping(t, responseSet[0])["value"] != "max-age=63072000; includeSubDomains; preload" {
		t.Fatal("client identity must preserve strict transport security")
	}
	filter := mapping(t, filters[1])
	if filter["type"] != "RequestHeaderModifier" {
		t.Fatal("the gateway must rewrite client identity")
	}
	headers := mapping(t, filter["requestHeaderModifier"])
	if headers["add"] != nil {
		t.Fatal("appending lets an earlier client header select the identity")
	}
	set := sequence(t, headers["set"])
	if len(set) != 1 || mapping(t, set[0])["name"] != "Forwarded" || mapping(t, set[0])["value"] != `for="%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%"` {
		t.Fatal("forwarding must replace the client value with connection metadata")
	}
	removed := map[any]bool{}
	for _, value := range sequence(t, headers["remove"]) {
		removed[value] = true
	}
	if !removed["X-Forwarded-For"] || !removed["X-Real-IP"] {
		t.Fatal("untrusted alternate client-address headers remain")
	}
	backends := sequence(t, rule["backendRefs"])
	if len(backends) != 1 || mapping(t, backends[0])["name"] != "private-mail" || mapping(t, backends[0])["port"] != 8080 {
		t.Fatal("client identity must retain the existing management backend")
	}
}

func TestClientIdentityRemainsInactiveInProduction(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	route := object(t, objects, "HTTPRoute", "private-mail")
	for _, value := range sequence(t, mapping(t, route["spec"])["rules"]) {
		for _, filter := range sequence(t, mapping(t, value)["filters"]) {
			if mapping(t, filter)["type"] == "RequestHeaderModifier" {
				t.Fatal("prepared forwarding must not activate before native trust and rollback proof")
			}
		}
	}
}
