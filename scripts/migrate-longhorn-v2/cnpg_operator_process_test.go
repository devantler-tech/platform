package main

import (
	"strings"
	"testing"
)

// operatorProcessFixture models the pinned two-replica manager deployment and one owned process.
func operatorProcessFixture() (object, object, object, object) {
	meta := func(name, uid string) object {
		return object{"name": name, "namespace": "cnpg-system", "uid": uid, "resourceVersion": "10", "generation": float64(1)}
	}
	owner := func(kind, name, uid string) []any {
		return []any{object{"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": uid, "controller": true, "blockOwnerDeletion": true}}
	}
	container := object{"name": "manager", "image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2", "command": []any{"/manager"}, "args": []any{"controller", "--leader-elect", "--max-concurrent-reconciles=10", "--config-map-name=cnpg-controller-manager-config", "--webhook-port=9443"}, "env": []any{object{"name": "OPERATOR_IMAGE_NAME", "value": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2"}, object{"name": "OPERATOR_NAMESPACE", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.namespace"}}}, object{"name": "MONITORING_QUERIES_CONFIGMAP", "value": "cnpg-default-monitoring"}}}
	dep := object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta("cloudnative-pg", "deployment-uid"), "spec": object{"replicas": float64(2), "template": object{"spec": object{"containers": []any{container}}}}, "status": object{"observedGeneration": float64(1), "replicas": float64(2), "readyReplicas": float64(2), "updatedReplicas": float64(2), "availableReplicas": float64(2)}}
	rsMeta := meta("cloudnative-pg-fixture", "replicaset-uid")
	rsMeta["ownerReferences"] = owner("Deployment", "cloudnative-pg", "deployment-uid")
	rs := object{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": rsMeta, "spec": object{"replicas": float64(2)}, "status": object{"observedGeneration": float64(1), "replicas": float64(2), "readyReplicas": float64(2), "availableReplicas": float64(2)}}
	podMeta := meta("cloudnative-pg-fixture-one", "pod-uid")
	podMeta["ownerReferences"] = owner("ReplicaSet", "cloudnative-pg-fixture", "replicaset-uid")
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": podMeta, "spec": object{"nodeName": "worker", "containers": []any{container}}, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}, "containerStatuses": []any{object{"name": "manager", "ready": true, "restartCount": float64(0), "imageID": "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:f96f6be3d81ec93819b443c665c575c609342fca840df90d4d87dae314a5b913", "containerID": "containerd://" + strings.Repeat("a", 64), "state": object{"running": object{"startedAt": "2026-11-03T10:14:19Z"}}}}}}
	node := object{"apiVersion": "v1", "kind": "Node", "metadata": object{"name": "worker", "uid": "node-uid", "resourceVersion": "10"}, "status": object{"conditions": []any{object{"type": "Ready", "status": "True"}}, "nodeInfo": object{"operatingSystem": "linux", "architecture": "amd64", "bootID": "boot-uid"}}}
	return pod, dep, rs, node
}

// TestCNPGManagerProcessRequiresTheReleasedImageAndNativeControllerChain checks positive binding.
func TestCNPGManagerProcessRequiresTheReleasedImageAndNativeControllerChain(t *testing.T) {
	pod, dep, rs, node := operatorProcessFixture()
	bound, err := bindCNPGOperatorProcess(pod, dep, rs, node)
	if err != nil || bound.PodUID != "pod-uid" || bound.DeploymentUID != "deployment-uid" || bound.SpecSHA == "" || bound.StartedAt == "" {
		t.Fatalf("complete manager process refused: %+v %v", bound, err)
	}
}

// TestCNPGManagerProcessRefusesWrongOwnershipReleaseAndRuntimeState prevents ambiguous startup provenance.
func TestCNPGManagerProcessRefusesWrongOwnershipReleaseAndRuntimeState(t *testing.T) {
	for name, mutate := range map[string]func(object, object, object, object){
		"missing node status": func(_, _, _, n object) { delete(n, "status") },
		"missing node name": func(p, _, _, n object) {
			delete(n["metadata"].(object), "name")
			delete(p["spec"].(object), "nodeName")
		},
		"foreign parent": func(p, _, _, _ object) {
			value(p, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "foreign"
		},
		"foreign deployment": func(_, _, r, _ object) {
			value(r, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "foreign"
		},
		"restarting": func(p, _, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["state"] = object{"waiting": object{"reason": "restart"}}
		},
		"unready":            func(p, _, _, _ object) { value(p, "status", "containerStatuses").([]any)[0].(object)["ready"] = false },
		"rolling deployment": func(_, d, _, _ object) { value(d, "status").(object)["updatedReplicas"] = float64(1) },
		"wrong image": func(p, _, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:" + strings.Repeat("b", 64)
		},
		"wrapper": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["command"] = []any{"/bin/sh", "-c", "/manager"}
		},
		"mounted executable": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["volumeMounts"] = []any{object{"name": "override", "mountPath": "/manager"}}
		},
		"env override": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["envFrom"] = []any{object{"configMapRef": object{"name": "foreign"}}}
		},
		"unknown architecture": func(_, _, _, n object) { value(n, "status", "nodeInfo").(object)["architecture"] = "unknown" },
		"missing boot":         func(_, _, _, n object) { delete(value(n, "status", "nodeInfo").(object), "bootID") },
	} {
		t.Run(name, func(t *testing.T) {
			pod, dep, rs, node := operatorProcessFixture()
			mutate(pod, dep, rs, node)
			if bound, err := bindCNPGOperatorProcess(pod, dep, rs, node); err == nil || bound != (cnpgOperatorProcess{}) {
				t.Fatal("unbound manager process accepted")
			}
		})
	}
}
