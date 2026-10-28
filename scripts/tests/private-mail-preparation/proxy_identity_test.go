package preparation_test

import "testing"

func TestProxyIdentityKeepsNativeBackendFencedAndProxyCredentialless(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-proxy-identity")
	service := mapping(t, object(t, objects, "Service", "private-mail")["spec"])
	ports := sequence(t, service["ports"])
	if len(ports) != 1 || mapping(t, ports[0])["port"] != 8080 || mapping(t, ports[0])["targetPort"] != "client-proxy" {
		t.Fatal("existing administration service must target only the client proxy")
	}
	pod := mapping(t, mapping(t, mapping(t, object(t, objects, "StatefulSet", "private-mail")["spec"])["template"])["spec"])
	initializers := sequence(t, pod["initContainers"])
	if len(initializers) != 1 {
		t.Fatal("proxy configuration must retain the single managed initializer")
	}
	args := sequence(t, mapping(t, initializers[0])["args"])
	if len(args) != 1 || args[0] != "--proxy-client-ip=true" {
		t.Fatal("the opt-in must declare transport activation without rewriting private settings")
	}
	var proxy map[string]any
	var nativeImage any
	for _, item := range sequence(t, pod["containers"]) {
		container := mapping(t, item)
		if container["name"] == "client-proxy" {
			proxy = container
		}
		if container["name"] == "private-mail" {
			nativeImage = container["image"]
		}
	}
	if proxy == nil {
		t.Fatal("missing maintained client proxy")
	}
	if nativeImage == nil || proxy["image"] != nativeImage {
		t.Fatal("proxy and native process must share the explicitly pinned bootstrap image")
	}
	mounts := sequence(t, proxy["volumeMounts"])
	if len(mounts) != 1 || mapping(t, mounts[0])["name"] != "client-proxy-config" || mapping(t, mounts[0])["readOnly"] != true {
		t.Fatal("proxy must receive configuration without credentials, TLS keys or mailbox data")
	}
	policy := mapping(t, object(t, objects, "CiliumNetworkPolicy", "private-mail")["spec"])
	allowed := sequence(t, policy["ingress"])
	if len(allowed) != 1 {
		t.Fatal("unexpected administration access path")
	}
	rule := mapping(t, allowed[0])
	entities := sequence(t, rule["fromEntities"])
	allowedPorts := sequence(t, mapping(t, sequence(t, rule["toPorts"])[0])["ports"])
	if len(entities) != 1 || entities[0] != "ingress" || len(allowedPorts) != 1 || mapping(t, allowedPorts[0])["port"] != "8081" {
		t.Fatal("only the gateway identity may reach the frontend")
	}
	denied := map[any]bool{}
	for _, item := range sequence(t, policy["ingressDeny"]) {
		for _, set := range sequence(t, mapping(t, item)["toPorts"]) {
			for _, port := range sequence(t, mapping(t, set)["ports"]) {
				denied[mapping(t, port)["port"]] = true
			}
		}
	}
	for _, port := range []string{"8080", "18080", "18081", "1025", "1587", "1993"} {
		if !denied[port] {
			t.Fatalf("native/recovery listener %s is not fenced", port)
		}
	}
}

func TestMaintainedProxyRemainsInactiveInProduction(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	pod := mapping(t, mapping(t, mapping(t, object(t, objects, "StatefulSet", "private-mail")["spec"])["template"])["spec"])
	for _, item := range sequence(t, pod["containers"]) {
		if mapping(t, item)["name"] == "client-proxy" {
			t.Fatal("proxy must not activate before native and production trust proof")
		}
	}
}
