package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestBenchmarkAdmissionRequiresEveryServingAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(nodes, pods, slices []object)
		ok     bool
	}{
		{name: "all three kube-apiserver mirrors and endpoints", ok: true},
		{name: "stale service roster", change: func(_, _ []object, slices []object) {
			slices[0]["endpoints"] = value(slices[0], "endpoints").([]any)[:2]
		}},
		{name: "wrong Kubernetes node epoch", change: func(_, pods, _ []object) {
			value(pods[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "replacement-node"
		}},
		{name: "non-host API process", change: func(_, pods, _ []object) {
			valueObject(pods[0], "spec")["hostNetwork"] = false
		}},
		{name: "API process epoch unavailable", change: func(_, pods, _ []object) {
			delete(valueObject(pods[0], "status"), "containerStatuses")
		}},
		{name: "duplicate direct transport", change: func(nodes, _, _ []object) {
			value(nodes[0], "status", "addresses").([]any)[1].(object)["address"] = "203.0.113.2"
		}},
		{name: "unready endpoint", change: func(_, _ []object, slices []object) {
			valueObject(value(slices[0], "endpoints").([]any)[0].(object), "conditions")["ready"] = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, pods, slices := benchmarkAdmissionFixture()
			if tc.change != nil {
				tc.change(nodes, pods, slices)
			}
			roster, err := qualifyBenchmarkAPIServers(nodes, pods, slices)
			if tc.ok {
				if err != nil || len(roster) != 3 || roster[0].Node != "prod-control-plane-1" ||
					roster[0].ExternalIP.String() != "203.0.113.1" {
					t.Fatalf("unexpected serving API roster: %v", err)
				}
			} else if err == nil {
				t.Fatal("admission proof accepted an incomplete or ambiguous serving API roster")
			}
		})
	}
}

func benchmarkAdmissionFixture() (nodes, pods, slices []object) {
	endpoints := make([]any, 0, 3)
	for i := 1; i <= 3; i++ {
		name, uid := fmt.Sprintf("prod-control-plane-%d", i), fmt.Sprintf("node-uid-%d", i)
		internal, external := fmt.Sprintf("10.0.0.%d", i), fmt.Sprintf("203.0.113.%d", i)
		nodes = append(nodes, object{
			"metadata": object{"name": name, "uid": uid, "labels": object{"node-role.kubernetes.io/control-plane": ""}},
			"spec":     object{"providerID": fmt.Sprintf("hcloud://%d", 1000+i)},
			"status": object{
				"nodeInfo":   object{"bootID": fmt.Sprintf("boot-%d", i)},
				"conditions": readyConditions(),
				"addresses": []any{
					object{"type": "InternalIP", "address": internal},
					object{"type": "ExternalIP", "address": external},
				},
			},
		})
		pods = append(pods, object{
			"metadata": object{
				"name": "kube-apiserver-" + name, "namespace": "kube-system", "uid": "pod-" + uid,
				"annotations": object{"kubernetes.io/config.mirror": "native"},
				"ownerReferences": []any{object{
					"apiVersion": "v1", "kind": "Node", "name": name, "uid": uid, "controller": true,
				}},
			},
			"spec": object{"nodeName": name, "hostNetwork": true},
			"status": object{
				"phase": "Running", "conditions": readyConditions(), "hostIP": internal, "podIP": internal,
				"containerStatuses": []any{object{
					"name": "kube-apiserver", "ready": true, "started": true,
					"containerID": "containerd://" + strings.Repeat(fmt.Sprint(i), 64),
					"state":       object{"running": object{"startedAt": "2026-11-10T10:00:00Z"}},
				}},
			},
		})
		endpoints = append(endpoints, object{"addresses": []any{internal}, "conditions": object{"ready": true}})
	}
	return nodes, pods, []object{{
		"metadata":    object{"name": "kubernetes", "namespace": "default", "uid": "slice", "labels": object{"kubernetes.io/service-name": "kubernetes"}},
		"addressType": "IPv4",
		"ports":       []any{object{"name": "https", "protocol": "TCP", "port": float64(6443)}},
		"endpoints":   endpoints,
	}}
}
