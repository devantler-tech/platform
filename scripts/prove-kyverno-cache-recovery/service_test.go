package main

import "testing"

func serviceFixture(t *testing.T) (object, object, []object) {
	t.Helper()
	d, p, r, pods := controllerFixture()
	bindings, err := qualifyControllers(encoded(t, d), encoded(t, p), nativeList(t, "apps/v1", "ReplicaSetList", []object{r}), nativeList(t, "v1", "PodList", pods))
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if text(binding, "metadata", "uid") == "pod-a" {
			binding["status"].(object)["podIP"] = "10.0.0.1"
		}
		if text(binding, "metadata", "uid") == "pod-b" {
			binding["status"].(object)["podIP"] = "10.0.0.2"
		}
	}
	service := object{"apiVersion": "v1", "kind": "Service", "metadata": identity("kyverno-svc", "service-1"), "spec": object{"type": "ClusterIP", "selector": object{"app.kubernetes.io/component": "admission-controller"}, "ports": []any{object{"name": "https", "protocol": "TCP", "port": 443, "targetPort": "https"}}}}
	slice := object{"metadata": identity("kyverno-svc-synthetic", "slice-1"), "addressType": "IPv4", "ports": []any{object{"name": "https", "protocol": "TCP", "port": 9443}}, "endpoints": []any{
		object{"addresses": []any{"10.0.0.1"}, "conditions": object{"ready": true, "serving": true, "terminating": false}, "targetRef": object{"kind": "Pod", "namespace": "kyverno", "name": "kyverno-admission-controller-hash-a", "uid": "pod-a"}},
		object{"addresses": []any{"10.0.0.2"}, "conditions": object{"ready": true, "serving": true, "terminating": false}, "targetRef": object{"kind": "Pod", "namespace": "kyverno", "name": "kyverno-admission-controller-hash-b", "uid": "pod-b"}},
	}}
	slice["metadata"].(object)["ownerReferences"] = owned("Service", "service-1")
	slice["metadata"].(object)["labels"] = object{"kubernetes.io/service-name": "kyverno-svc"}
	return service, slice, bindings
}

func TestWebhookServiceRoutesOnlyToTheQualifiedProcesses(t *testing.T) {
	s, e, bindings := serviceFixture(t)
	if got, err := qualifyService(encoded(t, s), nativeList(t, "discovery.k8s.io/v1", "EndpointSliceList", []object{e}), bindings); err != nil || len(got) != 2 {
		t.Fatalf("matching service endpoints rejected: %v", err)
	}
}

func TestForeignOrUnavailableEndpointCannotPass(t *testing.T) {
	for name, change := range map[string]func(object, object){
		"wrong service selector": func(s, e object) { s["spec"].(object)["selector"] = object{"foreign": "true"} },
		"external service target": func(s, e object) {
			s["spec"].(object)["type"] = "ExternalName"
			s["spec"].(object)["externalName"] = "foreign.invalid"
		},
		"wrong service port":  func(s, e object) { s["spec"].(object)["ports"].([]any)[0].(object)["targetPort"] = "foreign" },
		"foreign slice owner": func(s, e object) { e["metadata"].(object)["ownerReferences"] = owned("Service", "foreign") },
		"foreign Pod UID":     func(s, e object) { e["endpoints"].([]any)[0].(object)["targetRef"].(object)["uid"] = "foreign" },
		"foreign address":     func(s, e object) { e["endpoints"].([]any)[0].(object)["addresses"] = []any{"10.0.0.99"} },
		"unready endpoint":    func(s, e object) { e["endpoints"].([]any)[0].(object)["conditions"].(object)["ready"] = false },
		"wrong endpoint port": func(s, e object) { e["ports"].([]any)[0].(object)["port"] = 9444 },
	} {
		t.Run(name, func(t *testing.T) {
			s, e, bindings := serviceFixture(t)
			change(s, e)
			if _, err := qualifyService(encoded(t, s), nativeList(t, "discovery.k8s.io/v1", "EndpointSliceList", []object{e}), bindings); err == nil {
				t.Fatal("unqualified service routing accepted")
			}
		})
	}
}
