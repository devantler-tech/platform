package preparation_test

import (
	"reflect"
	"testing"
)

// Production opens SMTP, submission and IMAPS to the mail server's own load
// balancer alone, at exactly the private address it is pinned to and that the
// server trusts for PROXY headers. Management and the recovery and adapter
// listeners stay denied from every source.
func TestProductionAdmitsMailPortsFromItsLoadBalancerOnly(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	service := object(t, objects, "Service", "private-mail-public")
	address, ok := mapping(t, mapping(t, service["metadata"])["annotations"])["load-balancer.hetzner.cloud/private-ipv4"].(string)
	if !ok || address == "" || mapping(t, service["spec"])["externalTrafficPolicy"] != "Local" {
		t.Fatal("the mail load balancer must keep a pinned private address as the source")
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

	admitted := 0
	for _, item := range sequence(t, policy["ingress"]) {
		rule := mapping(t, item)
		switch {
		case rule["fromEntities"] != nil:
			if !reflect.DeepEqual(rule["fromEntities"], []any{"ingress"}) || !reflect.DeepEqual(ports(rule), map[any]bool{"8081": true}) {
				t.Fatal("only the gateway identity may reach the administration adapter")
			}
		case rule["fromCIDR"] != nil:
			admitted++
			if !reflect.DeepEqual(rule["fromCIDR"], []any{address + "/32"}) {
				t.Fatalf("mail ports must be admitted from the load balancer's %s/32 only, got %v", address, rule["fromCIDR"])
			}
			if !reflect.DeepEqual(ports(rule), map[any]bool{"1025": true, "1587": true, "1993": true}) {
				t.Fatal("the load balancer may reach the three mail listeners only")
			}
		default:
			t.Fatalf("unexpected ingress source: %v", rule)
		}
	}
	if admitted != 1 {
		t.Fatalf("want exactly one load-balancer rule, got %d", admitted)
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
