package main

import (
	"encoding/json"
	"testing"
)

func encoded(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func identity(name, uid string) object {
	return object{"name": name, "namespace": "kyverno", "uid": uid, "resourceVersion": "7", "generation": 3}
}
func owned(kind, uid string) []any {
	return []any{object{"kind": kind, "uid": uid, "controller": true}}
}
func cacheSpec() object {
	return object{"volumes": []any{object{"name": "sigstore", "emptyDir": object{}}}, "containers": []any{object{"name": "kyverno", "image": "registry.invalid/kyverno@sha256:synthetic", "env": []any{object{"name": "TUF_ROOT", "value": "/.sigstore"}}, "volumeMounts": []any{object{"name": "sigstore", "mountPath": "/.sigstore"}}}}}
}
func controllerFixture() (object, object, object, []object) {
	labels := object{"app.kubernetes.io/component": "admission-controller"}
	selector := object{"matchLabels": labels}
	template := object{"metadata": object{"labels": labels, "annotations": object{"cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes": "sigstore"}}, "spec": cacheSpec()}
	deployment := object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": identity("kyverno-admission-controller", "deployment-1"), "spec": object{"replicas": 2, "selector": selector, "template": template}, "status": object{"observedGeneration": 3, "readyReplicas": 2, "availableReplicas": 2, "updatedReplicas": 2}}
	pdb := object{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget", "metadata": identity("kyverno-admission-controller", "pdb-1"), "spec": object{"minAvailable": 1, "selector": selector}, "status": object{"observedGeneration": 3, "currentHealthy": 2, "desiredHealthy": 1, "disruptionsAllowed": 1}}
	rs := object{"metadata": identity("kyverno-admission-controller-hash", "rs-1"), "spec": object{"template": template}}
	rs["metadata"].(object)["ownerReferences"] = owned("Deployment", "deployment-1")
	var pods []object
	for _, name := range []string{"a", "b"} {
		metadata := identity("kyverno-admission-controller-hash-"+name, "pod-"+name)
		metadata["labels"] = labels
		metadata["annotations"] = object{"cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes": "sigstore"}
		metadata["ownerReferences"] = owned("ReplicaSet", "rs-1")
		spec := cacheSpec()
		spec["nodeName"] = "synthetic-worker-" + name
		pods = append(pods, object{"metadata": metadata, "spec": spec, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}, "containerStatuses": []any{object{"name": "kyverno", "ready": true, "started": true, "restartCount": 0, "containerID": "synthetic-container-" + name, "imageID": "synthetic-image", "state": object{"running": object{"startedAt": "2026-11-02T09:00:00Z"}}}}}})
	}
	return deployment, pdb, rs, pods
}
func nativeList(t *testing.T, version, kind string, items any) []byte {
	return encoded(t, object{"apiVersion": version, "kind": kind, "metadata": object{"resourceVersion": "77"}, "items": items})
}

func TestRecoveredAdmissionRetainsReplicaAndDisruptionFloors(t *testing.T) {
	d, p, r, pods := controllerFixture()
	got, err := qualifyControllers(encoded(t, d), encoded(t, p), nativeList(t, "apps/v1", "ReplicaSetList", []object{r}), nativeList(t, "v1", "PodList", pods))
	if err != nil || len(got) != 5 {
		t.Fatalf("qualified admission service rejected: bindings=%d err=%v", len(got), err)
	}
}

func TestUnavailableOrUnownedAdmissionCannotPass(t *testing.T) {
	for name, change := range map[string]func(object, object, object, []object){
		"one replica": func(d, p, r object, pods []object) { d["status"].(object)["readyReplicas"] = 1 },
		"foreign executable": func(d, p, r object, pods []object) {
			pods[0]["spec"].(object)["containers"].([]any)[0].(object)["image"] = "registry.invalid/foreign:latest"
		},
		"arguments rewritten": func(d, p, r object, pods []object) {
			pods[0]["spec"].(object)["containers"].([]any)[0].(object)["args"] = []any{"--foreign"}
		},
		"unprotected disruption": func(d, p, r object, pods []object) { p["spec"].(object)["minAvailable"] = 0 },
		"unavailable PDB":        func(d, p, r object, pods []object) { p["status"].(object)["disruptionsAllowed"] = 0 },
		"foreign owner": func(d, p, r object, pods []object) {
			pods[0]["metadata"].(object)["ownerReferences"] = owned("ReplicaSet", "foreign-rs")
		},
		"unready process": func(d, p, r object, pods []object) {
			pods[0]["status"].(object)["containerStatuses"].([]any)[0].(object)["ready"] = false
		},
		"blank process identity": func(d, p, r object, pods []object) {
			pods[0]["status"].(object)["containerStatuses"].([]any)[0].(object)["containerID"] = ""
		},
		"blanket eviction": func(d, p, r object, pods []object) {
			pods[0]["metadata"].(object)["annotations"].(object)["cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"] = "sigstore,data"
		},
		"persistent cache": func(d, p, r object, pods []object) {
			pods[0]["spec"].(object)["volumes"] = []any{object{"name": "sigstore", "persistentVolumeClaim": object{"claimName": "data"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, p, r, pods := controllerFixture()
			change(d, p, r, pods)
			if _, err := qualifyControllers(encoded(t, d), encoded(t, p), nativeList(t, "apps/v1", "ReplicaSetList", []object{r}), nativeList(t, "v1", "PodList", pods)); err == nil {
				t.Fatal("unsafe admission state accepted")
			}
		})
	}
}
