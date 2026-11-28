package main

import (
	"fmt"
	"testing"
)

func TestOpenBaoMigrationKeepsBothVotersAndTheWriterRoute(t *testing.T) {
	for _, fault := range []string{"", "target is writer", "survivor replaced", "worker replaced", "one peer unready",
		"another writer address", "foreign service owner", "active endpoint terminating", "two active endpoints"} {
		t.Run(fault, func(t *testing.T) {
			st, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
			bound, err := bindOpenBaoProviderStorage("openbao-1", st, pods, claims, pvs, attachments)
			if err != nil {
				t.Fatal(err)
			}
			objects := make([]object, 2)
			nodes := make([]object, 2)
			for i, ordinal := range []int{0, 2} {
				_, replica, _, _, _ := openBaoProviderStorageFixture()
				pod := replica[0]
				valueObject(pod, "metadata")["name"] = fmt.Sprintf("openbao-%d", ordinal)
				valueObject(pod, "metadata")["uid"] = fmt.Sprintf("b286e4f7-51db-4bc1-aa4d-%012d", 110+i)
				valueObject(pod, "spec")["nodeName"] = fmt.Sprintf("worker-%d", ordinal)
				valueObject(pod, "status")["podIP"] = fmt.Sprintf("192.0.2.%d", 110+i)
				objects[i] = pod
				nodes[i] = object{"apiVersion": "v1", "kind": "Node",
					"metadata": object{"name": text(pod, "spec", "nodeName"), "uid": fmt.Sprintf("b286e4f7-51db-4bc1-aa4d-%012d", 120+i), "resourceVersion": "9"},
					"status":   object{"conditions": []any{object{"type": "Ready", "status": "True"}}}}
			}
			peers, err := bindOpenBaoSurvivors(bound, st, objects, nodes)
			if err != nil {
				t.Fatal(err)
			}
			const svcUID = "b286e4f7-51db-4bc1-aa4d-000000000200"
			route := object{"apiVersion": "v1", "kind": "Service",
				"metadata": object{"name": "openbao-active", "namespace": openBaoNamespace, "uid": svcUID, "resourceVersion": "8"},
				"spec":     object{"selector": object{"openbao-active": "true"}}}
			endpoint := object{
				"addresses":  []any{text(objects[0], "status", "podIP")},
				"conditions": object{"ready": true, "serving": true, "terminating": false},
				"nodeName":   peers[0].Node.Name,
				"targetRef":  object{"kind": "Pod", "namespace": openBaoNamespace, "name": peers[0].Pod.Name, "uid": peers[0].Pod.UID},
			}
			slice := object{"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
				"metadata": object{"name": "openbao-active-fixture", "namespace": openBaoNamespace,
					"uid": "b286e4f7-51db-4bc1-aa4d-000000000201", "resourceVersion": "8",
					"labels":          object{"kubernetes.io/service-name": "openbao-active", "endpointslice.kubernetes.io/managed-by": "endpointslice-controller.k8s.io"},
					"ownerReferences": []any{object{"apiVersion": "v1", "kind": "Service", "name": "openbao-active", "uid": svcUID, "controller": true}}},
				"addressType": "IPv4", "endpoints": []any{endpoint}}
			switch fault {
			case "target is writer":
				valueObject(endpoint, "targetRef")["name"] = bound.Member.Name
				valueObject(endpoint, "targetRef")["uid"] = bound.Member.UID
			case "survivor replaced":
				valueObject(objects[1], "metadata")["uid"] = "b286e4f7-51db-4bc1-aa4d-000000000555"
			case "worker replaced":
				valueObject(nodes[1], "metadata")["uid"] = "b286e4f7-51db-4bc1-aa4d-000000000556"
			case "one peer unready":
				valueObject(objects[1], "status")["phase"] = "Pending"
			case "another writer address":
				endpoint["addresses"] = []any{"192.0.2.211"}
			case "foreign service owner":
				valueObject(route, "metadata")["uid"] = "b286e4f7-51db-4bc1-aa4d-000000000559"
			case "active endpoint terminating":
				valueObject(endpoint, "conditions")["terminating"] = true
			case "two active endpoints":
				slice["endpoints"] = []any{endpoint, endpoint}
			}
			if err := verifyOpenBaoSurvivors(bound, st, peers, objects, nodes, route, []object{slice}); (err != nil) != (fault != "") {
				t.Fatalf("survivor and writer route proof for %q: %v", fault, err)
			}
		})
	}
}
