package preparation_test

import (
	"reflect"
	"testing"
)

// Production opens SMTP, both submission ports and IMAPS to the mail server's
// own load balancer alone, at exactly the private address it is pinned to and
// that the server trusts for PROXY headers. Management and the recovery and
// adapter listeners stay denied from every source.
func TestProductionAdmitsMailPortsFromItsLoadBalancerOnly(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	service := object(t, objects, "Service", "private-mail-public")
	address, ok := mapping(t, mapping(t, service["metadata"])["annotations"])["load-balancer.hetzner.cloud/private-ipv4"].(string)
	if !ok || address == "" || mapping(t, service["spec"])["externalTrafficPolicy"] != "Local" {
		t.Fatal("the mail load balancer must keep a pinned private address as the source")
	}
	// Each public port reaches its own listener: 465 is implicit-TLS
	// submission (RFC 8314), which the server's autoconfig offers.
	published := map[any]any{}
	for _, item := range sequence(t, mapping(t, service["spec"])["ports"]) {
		port := mapping(t, item)
		if port["protocol"] != nil && port["protocol"] != "TCP" {
			t.Fatalf("unexpected protocol on public port %v", port["port"])
		}
		published[port["port"]] = port["targetPort"]
	}
	if !reflect.DeepEqual(published, map[any]any{25: 1025, 587: 1587, 465: 1465, 993: 1993}) {
		t.Fatalf("the mail load balancer must publish SMTP, both submission ports and IMAPS only, got %v", published)
	}
	policy := mapping(t, object(t, objects, "CiliumNetworkPolicy", "private-mail")["spec"])

	ports := func(rule map[string]any) map[any]bool {
		found := map[any]bool{}
		for _, set := range sequence(t, rule["toPorts"]) {
			for _, port := range sequence(t, mapping(t, set)["ports"]) {
				p := mapping(t, port)
				if p["protocol"] != "TCP" {
					t.Fatalf("unexpected protocol on port %v", p["port"])
				}
				found[p["port"]] = true
			}
		}
		return found
	}

	admitted, importer := 0, 0
	for _, item := range sequence(t, policy["ingress"]) {
		rule := mapping(t, item)
		switch {
		// The one-time mailbox history copy (#5481) may reach IMAPS alone, from
		// its own pod label in this namespace, for Cilium's in-cluster delivery
		// to the load balancer address.
		case rule["fromEndpoints"] != nil:
			importer++
			want := []any{map[string]any{"matchLabels": map[string]any{"app": "email-history-import", "k8s:io.kubernetes.pod.namespace": "private-mail"}}}
			if !reflect.DeepEqual(rule["fromEndpoints"], want) || !reflect.DeepEqual(ports(rule), map[any]bool{"1993": true}) {
				t.Fatalf("only the history copy's pods may reach IMAPS in-cluster, got %v", rule)
			}
		case rule["fromEntities"] != nil:
			if !reflect.DeepEqual(rule["fromEntities"], []any{"ingress"}) || !reflect.DeepEqual(ports(rule), map[any]bool{"8081": true}) {
				t.Fatal("only the gateway identity may reach the administration adapter")
			}
		case rule["fromCIDR"] != nil:
			admitted++
			if !reflect.DeepEqual(rule["fromCIDR"], []any{address + "/32"}) {
				t.Fatalf("mail ports must be admitted from the load balancer's %s/32 only, got %v", address, rule["fromCIDR"])
			}
			if !reflect.DeepEqual(ports(rule), map[any]bool{"1025": true, "1587": true, "1465": true, "1993": true}) {
				t.Fatal("the load balancer may reach the four mail listeners only")
			}
		default:
			t.Fatalf("unexpected ingress source: %v", rule)
		}
	}
	if admitted != 1 || importer > 1 {
		t.Fatalf("want exactly one load-balancer rule and at most one history-copy rule, got %d and %d", admitted, importer)
	}

	denied := map[any]bool{}
	for _, item := range sequence(t, policy["ingressDeny"]) {
		for port := range ports(mapping(t, item)) {
			denied[port] = true
		}
	}
	if !reflect.DeepEqual(denied, map[any]bool{"8080": true, "18080": true, "18081": true}) {
		t.Fatalf("management, recovery and adapter listeners must stay denied, got %v", denied)
	}
}
