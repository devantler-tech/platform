package main

import (
	"strings"
	"testing"
)

func recoveryAgentFixture() (object, object, object) {
	const nodeUID = "11111111-1111-4111-8111-111111111111"
	const dsUID = "22222222-2222-4222-8222-222222222222"
	image := recoveryAgentImage
	containers := []any{object{"name": "cilium-agent", "image": image}}
	node := object{"apiVersion": "v1", "kind": "Node", "metadata": object{"name": "synthetic-worker", "uid": nodeUID}, "status": object{"conditions": readyConditions(), "nodeInfo": object{"bootID": "33333333-3333-4333-8333-333333333333", "operatingSystem": "linux", "architecture": "amd64"}}}
	ds := object{"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": object{"name": "cilium", "namespace": "kube-system", "uid": dsUID, "generation": float64(7)},
		"spec":   object{"template": object{"spec": object{"containers": containers, "hostNetwork": true}}},
		"status": object{"observedGeneration": float64(7), "desiredNumberScheduled": float64(3), "currentNumberScheduled": float64(3), "updatedNumberScheduled": float64(3), "numberReady": float64(3), "numberMisscheduled": float64(0)}}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": "cilium-synthetic", "namespace": "kube-system", "uid": "44444444-4444-4444-8444-444444444444", "labels": object{"k8s-app": "cilium"},
		"ownerReferences": []any{object{"apiVersion": "apps/v1", "kind": "DaemonSet", "name": "cilium", "uid": dsUID, "controller": true}}},
		"spec":   object{"nodeName": "synthetic-worker", "hostNetwork": true, "containers": containers},
		"status": object{"phase": "Running", "conditions": readyConditions(), "containerStatuses": []any{object{"name": "cilium-agent", "imageID": "quay.io/cilium/cilium@" + recoveryAgentIndex, "containerID": "containerd://" + strings.Repeat("c", 64), "ready": true, "restartCount": float64(0), "state": object{"running": object{"startedAt": "2026-11-01T00:00:00Z"}}}}}}
	return pod, ds, node
}

func TestRecoveryCiliumAgentBindsDaemonSetNodeAndProcess(t *testing.T) {
	p, ds, n := recoveryAgentFixture()
	before, err := bindRecoveryCiliumAgent(p, ds, n)
	if err != nil || before.PodUID == "" || before.ContainerID == "" || before.NodeUID == "" || before.DaemonSetUID == "" {
		t.Fatalf("agent binding: %+v %v", before, err)
	}
	status := value(p, "status", "containerStatuses").([]any)[0].(object)
	status["containerID"] = "containerd://" + strings.Repeat("d", 64)
	status["restartCount"] = float64(1)
	after, err := bindRecoveryCiliumAgent(p, ds, n)
	if err != nil || after == before {
		t.Fatal("a restarted agent reused its prior process binding")
	}
}

func TestRecoveryCiliumAgentRefusesAmbiguousOrRollingObservations(t *testing.T) {
	for name, change := range map[string]func(object, object, object){
		"wrong node": func(p, _, _ object) { p["spec"].(object)["nodeName"] = "another-worker" },
		"foreign owner": func(p, _, _ object) {
			value(p, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "55555555-5555-4555-8555-555555555555"
		},
		"terminating Pod": func(p, _, _ object) { p["metadata"].(object)["deletionTimestamp"] = "2026-11-01T00:00:01Z" },
		"not ready":       func(p, _, _ object) { p["status"].(object)["conditions"] = []any{} },
		"agent not running": func(p, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["state"] = object{"waiting": object{}}
		},
		"fractional restarts": func(p, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(0.5)
		},
		"missing executable digest": func(p, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "unresolved"
		},
		"duplicate process": func(p, _, _ object) {
			list := value(p, "status", "containerStatuses").([]any)
			p["status"].(object)["containerStatuses"] = append(list, list[0])
		},
		"mutable declared image": func(p, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["image"] = "quay.io/cilium/cilium:v1.20.5"
		},
		"DaemonSet generation pending": func(_, d, _ object) { d["metadata"].(object)["generation"] = float64(8) },
		"DaemonSet rollout pending":    func(_, d, _ object) { d["status"].(object)["updatedNumberScheduled"] = float64(2) },
		"misscheduled agents":          func(_, d, _ object) { d["status"].(object)["numberMisscheduled"] = float64(1) },
		"missing misscheduled counter": func(_, d, _ object) { delete(d["status"].(object), "numberMisscheduled") },
		"null unavailable counter":     func(_, d, _ object) { d["status"].(object)["numberUnavailable"] = nil },
		"Pod image differs from DaemonSet": func(p, _, _ object) {
			p["spec"].(object)["containers"] = []any{object{"name": "cilium-agent", "image": "quay.io/cilium/cilium:v1.20.5@sha256:" + strings.Repeat("e", 64)}}
		},
		"node unready":               func(_, _ object, n object) { n["status"].(object)["conditions"] = []any{} },
		"missing node boot identity": func(_, _ object, n object) { value(n, "status", "nodeInfo").(object)["bootID"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			p, d, n := recoveryAgentFixture()
			change(p, d, n)
			if _, err := bindRecoveryCiliumAgent(p, d, n); err == nil {
				t.Fatal("ambiguous, unready, or rolling agent observation accepted")
			}
		})
	}
}

func TestRecoveryAgentContinuityIncludesReplacementAndBootIdentities(t *testing.T) {
	for name, change := range map[string]func(object, object, object){
		"Pod UID":  func(p, _, _ object) { p["metadata"].(object)["uid"] = "55555555-5555-4555-8555-555555555555" },
		"Node UID": func(_, _ object, n object) { n["metadata"].(object)["uid"] = "55555555-5555-4555-8555-555555555555" },
		"Node boot": func(_, _ object, n object) {
			value(n, "status", "nodeInfo").(object)["bootID"] = "55555555-5555-4555-8555-555555555555"
		},
		"DaemonSet generation": func(_, d, _ object) {
			d["metadata"].(object)["generation"] = float64(8)
			d["status"].(object)["observedGeneration"] = float64(8)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, d, n := recoveryAgentFixture()
			before, err := bindRecoveryCiliumAgent(p, d, n)
			if err != nil {
				t.Fatal(err)
			}
			change(p, d, n)
			after, err := bindRecoveryCiliumAgent(p, d, n)
			if err != nil || before == after {
				t.Fatalf("changed identity reused: %v", err)
			}
		})
	}
}
