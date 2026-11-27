package main

import (
	"encoding/json"
	"os"
	"testing"
)

func recoveryEndpointFixture(t *testing.T) ([]object, object, object) {
	t.Helper()
	var endpoints []object
	var cep object
	for path, target := range map[string]any{"testdata/synthetic-endpoint-get-cilium-1.20.5.json": &endpoints, "testdata/synthetic-cep-cilium-1.20.5.json": &cep} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, target); err != nil {
			t.Fatal(err)
		}
	}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": "restore-1", "namespace": "longhorn-db-proof-umami-db-1234", "uid": "11111111-2222-4333-8444-555555555555"}, "spec": object{"nodeName": "synthetic-worker"}, "status": object{"phase": "Running", "podIP": "192.0.2.17", "podIPs": []any{object{"ip": "192.0.2.17"}}}}
	return endpoints, cep, pod
}

func endpointBytes(t *testing.T, e []object) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecoveryEndpointUsesNativeRealizedPolicyAndCEPUIDOwnership(t *testing.T) {
	e, cep, pod := recoveryEndpointFixture(t)
	p, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), cep, pod, 817)
	if err != nil || p.EndpointID != 2718 || p.SecurityID != 1234 || p.PolicyRevision != 817 || p.PodUID != text(pod, "metadata", "uid") || p.CEPUID != text(cep, "metadata", "uid") || p.Attachment == "" || !p.Monitor.Notifications || p.Monitor.Audit {
		t.Fatalf("native endpoint binding: %+v %v", p, err)
	}
	// The native CEP Pod owner omits controller:true. It is still the exact owner.
	if value(value(cep, "metadata", "ownerReferences").([]any)[0].(object), "controller") != nil {
		t.Fatal("fixture invented a controller requirement")
	}
}

func TestRecoveryEndpointRefusesStaleOrAuditedDatapaths(t *testing.T) {
	for name, change := range map[string]func([]object, object, object){
		"CEP ready while native disconnects": func(e []object, _, _ object) { value(e[0], "status").(object)["state"] = "disconnecting" },
		"old policy": func(e []object, _, _ object) {
			value(e[0], "status", "policy", "realized").(object)["policy-revision"] = float64(816)
		},
		"ingress enforcement only": func(e []object, _, _ object) {
			value(e[0], "status", "policy", "realized").(object)["policy-enabled"] = "ingress"
		},
		"audit mode": func(e []object, _, _ object) {
			value(e[0], "status", "policy", "realized").(object)["policy-enabled"] = "audit-both"
		},
		"wrong realized security identity": func(e []object, _, _ object) {
			value(e[0], "status", "policy", "realized").(object)["id"] = float64(2718)
		},
		"different endpoint": func(e []object, _, _ object) { e[0]["id"] = float64(2719) },
		"quoted endpoint id": func(e []object, _, _ object) { e[0]["id"] = "2718" },
		"stale CEP identity": func(_ []object, c, _ object) { value(c, "status", "identity").(object)["id"] = float64(1235) },
		"different native IP": func(e []object, _, _ object) {
			value(e[0], "status", "networking", "addressing").([]any)[0].(object)["ipv4"] = "192.0.2.18"
		},
		"different CEP IP": func(_ []object, c, _ object) {
			value(c, "status", "networking", "addressing").([]any)[0].(object)["ipv4"] = "192.0.2.18"
		},
		"other namespace identity": func(e []object, _, _ object) {
			value(e[0], "status", "identity").(object)["labels"] = []any{"k8s:io.kubernetes.pod.namespace=production"}
		},
		"missing attachment": func(e []object, _, _ object) {
			delete(value(e[0], "status", "external-identifiers").(object), "cni-attachment-id")
		},
		"missing identity-bound options": func(e []object, _, _ object) {
			delete(value(e[0], "status").(object), "realized")
		},
		"identity-bound audit mode": func(e []object, _, _ object) {
			value(e[0], "status", "realized", "options").(object)["PolicyAuditMode"] = "Enabled"
		},
		"conflicting CEP attachment": func(_ []object, c, _ object) {
			value(c, "status").(object)["external-identifiers"] = object{"cni-attachment-id": "another-attachment:eth0"}
		},
		"malformed CEP attachment": func(_ []object, c, _ object) {
			value(c, "status").(object)["external-identifiers"] = object{"cni-attachment-id": float64(1)}
		},
		"null CEP attachment": func(_ []object, c, _ object) {
			value(c, "status").(object)["external-identifiers"] = object{"cni-attachment-id": nil}
		},
		"malformed CEP identifiers": func(_ []object, c, _ object) {
			value(c, "status").(object)["external-identifiers"] = "invalid"
		},
		"replaced Pod":    func(_ []object, _, p object) { p["metadata"].(object)["uid"] = "22222222-2222-4222-8222-222222222222" },
		"terminating CEP": func(_ []object, c, _ object) { c["metadata"].(object)["deletionTimestamp"] = "2026-11-01T00:01:00Z" },
		"missing node":    func(_ []object, _, p object) { delete(p["spec"].(object), "nodeName") },
		"host networking": func(_ []object, _, p object) { p["spec"].(object)["hostNetwork"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			e, c, p := recoveryEndpointFixture(t)
			change(e, c, p)
			if _, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), c, p, 817); err == nil {
				t.Fatal("unbound or unenforced endpoint accepted")
			}
		})
	}
	e, c, p := recoveryEndpointFixture(t)
	if _, err := bindRecoveryCiliumEndpoint(endpointBytes(t, append(e, e...)), c, p, 817); err == nil {
		t.Fatal("ambiguous endpoint array accepted")
	}
	if _, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), c, p, 0); err == nil {
		t.Fatal("missing import revision accepted")
	}
}

func TestRecoveryEndpointAcceptsMatchingOptionalCEPAttachment(t *testing.T) {
	e, c, p := recoveryEndpointFixture(t)
	value(c, "status").(object)["external-identifiers"] = object{"cni-attachment-id": text(e[0], "status", "external-identifiers", "cni-attachment-id")}
	if _, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), c, p, 817); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryEndpointRejectsAddressValuesInTheWrongFamilyField(t *testing.T) {
	for _, native := range []bool{false, true} {
		e, c, p := recoveryEndpointFixture(t)
		target := c
		if native {
			target = e[0]
		}
		pair := value(target, "status", "networking", "addressing").([]any)[0].(object)
		pair["ipv6"] = pair["ipv4"]
		delete(pair, "ipv4")
		if _, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), c, p, 817); err == nil {
			t.Fatal("an IPv4 address was accepted in an IPv6-only field")
		}
	}
}

func TestRecoveryEndpointBindsBothNativeAddressFamilies(t *testing.T) {
	e, c, p := recoveryEndpointFixture(t)
	for _, target := range []object{c, e[0]} {
		value(target, "status", "networking", "addressing").([]any)[0].(object)["ipv6"] = "2001:db8::17"
	}
	p["status"].(object)["podIPs"] = []any{object{"ip": "192.0.2.17"}, object{"ip": "2001:db8::17"}}
	proof, err := bindRecoveryCiliumEndpoint(endpointBytes(t, e), c, p, 817)
	if err != nil || proof.Addresses != "192.0.2.17,2001:db8::17" {
		t.Fatalf("complete dual-stack ownership was not bound: %v", err)
	}
}
