package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const controllerEvidence = `{"Ticket":"native-123-1","Journal":{"controllerUID":"deployment","controllerTicket":"native-123-1","controllerPodUIDs":["old-1","old-2"]},"Deployment":{"metadata":{"name":"arc-controller","namespace":"arc-systems","uid":"deployment","generation":3},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"platform.devantler.tech/arc-retirement-restart":"native-123-1"}},"spec":{"containers":[{"name":"manager","command":["/manager"],"args":["--watch-single-namespace=arc-runners","--auto-scaling-runner-set-only"]}]} }},"status":{"observedGeneration":3,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},"ReplicaSets":[{"metadata":{"uid":"new-rs","ownerReferences":[{"kind":"Deployment","uid":"deployment","controller":true}]}}],"Pods":[{"uid":"new-1","ownerUID":"new-rs","ownerKind":"ReplicaSet","phase":"Running","ready":"True","deleting":""}]}`

func TestEveryCapturedControllerProcessMustBeGone(t *testing.T) {
	if err := controllerProof([]byte(controllerEvidence)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"old-process", "extra-process", "unknown-owner", "stale-deployment", "replacement-deployment", "wrong-scope", "wrong-ticket", "deleting"} {
		var o map[string]any
		_ = json.Unmarshal([]byte(controllerEvidence), &o)
		pods := o["Pods"].([]any)
		pod := pods[0].(map[string]any)
		dep := o["Deployment"].(map[string]any)
		switch kind {
		case "old-process":
			pod["uid"] = "old-2"
		case "extra-process":
			o["Pods"] = append(pods, pod)
		case "unknown-owner":
			pod["ownerUID"] = "foreign"
		case "stale-deployment":
			dep["status"].(map[string]any)["observedGeneration"] = 2
		case "replacement-deployment":
			dep["metadata"].(map[string]any)["uid"] = "replacement"
		case "wrong-scope":
			dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"] = []string{"--watch-single-namespace=other", "--auto-scaling-runner-set-only"}
		case "wrong-ticket":
			o["Ticket"] = "other"
		case "deleting":
			pod["deleting"] = "now"
		}
		b, _ := json.Marshal(o)
		if controllerProof(b) == nil {
			t.Fatal("accepted", kind)
		}
	}
}
func TestRestorationJoinsEveryCurrentLayerToThePublishedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	ready := []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": 2}}
	ks := []any{}
	for _, name := range []string{"flux-system", "infrastructure-controllers", "infrastructure"} {
		ks = append(ks, map[string]any{"metadata": map[string]any{"name": name, "namespace": "flux-system", "uid": name, "generation": 2, "annotations": map[string]any{"reconcile.fluxcd.io/requestedAt": "native-123-1"}}, "spec": map[string]any{"sourceRef": map[string]any{"kind": "OCIRepository", "name": "flux-system"}}, "status": map[string]any{"observedGeneration": 2, "lastAppliedRevision": "latest@" + digest, "lastAttemptedRevision": "latest@" + digest, "lastHandledReconcileAt": "native-123-1", "conditions": ready}})
	}
	o := map[string]any{"Digest": digest, "Ticket": "native-123-1", "OCI": map[string]any{"metadata": map[string]any{"name": "flux-system", "namespace": "flux-system", "uid": "source", "generation": 2}, "spec": map[string]any{"verify": map[string]any{"provider": "cosign"}}, "status": map[string]any{"observedGeneration": 2, "artifact": map[string]any{"digest": digest}, "conditions": ready}}, "Kustomizations": ks}
	b, _ := json.Marshal(o)
	var signed map[string]any
	_ = json.Unmarshal(b, &signed)
	ociStatus := signed["OCI"].(map[string]any)["status"].(map[string]any)
	ociStatus["conditions"] = append(ociStatus["conditions"].([]any), map[string]any{"type": "SourceVerified", "status": "True", "observedGeneration": 2})
	b, _ = json.Marshal(signed)
	if err := sourceProof(b); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"stale-source", "wrong-layer", "stale-generation", "pending-request", "stale-attempt", "reconciling", "unsigned", "duplicate-layer"} {
		var changed map[string]any
		_ = json.Unmarshal(b, &changed)
		layers := changed["Kustomizations"].([]any)
		layer := layers[2].(map[string]any)
		status := layer["status"].(map[string]any)
		switch kind {
		case "stale-source":
			changed["OCI"].(map[string]any)["status"].(map[string]any)["artifact"].(map[string]any)["digest"] = "sha256:" + strings.Repeat("b", 64)
		case "wrong-layer":
			layer["metadata"].(map[string]any)["name"] = "apps"
		case "stale-generation":
			status["observedGeneration"] = 1
		case "pending-request":
			status["lastHandledReconcileAt"] = "old"
		case "stale-attempt":
			status["lastAttemptedRevision"] = "old"
		case "reconciling":
			status["conditions"] = append(status["conditions"].([]any), map[string]any{"type": "Reconciling", "status": "True"})
		case "unsigned":
			changed["OCI"].(map[string]any)["spec"] = map[string]any{}
		case "duplicate-layer":
			changed["Kustomizations"] = append(layers, layer)
		}
		bad, _ := json.Marshal(changed)
		if sourceProof(bad) == nil {
			t.Fatal("accepted", kind)
		}
	}
}
