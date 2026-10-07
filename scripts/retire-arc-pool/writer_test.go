package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func sourceFixture(t *testing.T) []byte {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	ready := []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": 2}}
	layers := []any{}
	for _, name := range []string{"flux-system", "infrastructure-controllers", "infrastructure"} {
		layers = append(layers, map[string]any{"metadata": map[string]any{"name": name, "namespace": "flux-system", "uid": name, "generation": 2, "annotations": map[string]any{"reconcile.fluxcd.io/requestedAt": "native-123-1"}}, "spec": map[string]any{"sourceRef": map[string]any{"kind": "OCIRepository", "name": "flux-system"}}, "status": map[string]any{"observedGeneration": 2, "lastAppliedRevision": "latest@" + digest, "lastAttemptedRevision": "latest@" + digest, "lastHandledReconcileAt": "native-123-1", "conditions": ready}})
	}
	body, err := json.Marshal(map[string]any{"Digest": digest, "Ticket": "native-123-1", "OCI": map[string]any{"metadata": map[string]any{"name": "flux-system", "namespace": "flux-system", "uid": "source", "generation": 2}, "spec": map[string]any{"verify": map[string]any{"provider": "cosign"}}, "status": map[string]any{"observedGeneration": 2, "artifact": map[string]any{"digest": digest}, "conditions": append(ready, map[string]any{"type": "SourceVerified", "status": "True", "observedGeneration": 2})}}, "Kustomizations": layers})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func writerFixture(t *testing.T) *writerReceipt {
	t.Helper()
	return &writerReceipt{Owner: fixture().Owner, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ticket: "native-123-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OCI: writerEpoch{UID: "source", Generation: 2}, Layers: []writerLayer{{Name: "flux-system", writerEpoch: writerEpoch{UID: "flux-system", Generation: 2}}, {Name: "infrastructure-controllers", writerEpoch: writerEpoch{UID: "infrastructure-controllers", Generation: 2}}, {Name: "infrastructure", writerEpoch: writerEpoch{UID: "infrastructure", Generation: 2}}}}
}
func fluxFixture() map[string]any {
	return map[string]any{
		"Deployment":  map[string]any{"metadata": map[string]any{"name": "kustomize-controller", "namespace": "flux-system", "uid": "flux-deployment", "generation": 3}, "spec": map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "manager", "image": reviewedFluxImage}}}}}, "status": map[string]any{"observedGeneration": 3, "replicas": 2, "updatedReplicas": 2, "readyReplicas": 2, "availableReplicas": 2}},
		"ReplicaSets": []any{map[string]any{"metadata": map[string]any{"uid": "flux-rs", "ownerReferences": []any{map[string]any{"kind": "Deployment", "uid": "flux-deployment", "controller": true}}}}},
		"Pods":        []any{map[string]any{"uid": "flux-pod-1", "ownerUID": "flux-rs", "ownerKind": "ReplicaSet", "phase": "Running", "ready": "True", "deleting": "", "imageID": reviewedFluxImageID}, map[string]any{"uid": "flux-pod-2", "ownerUID": "flux-rs", "ownerKind": "ReplicaSet", "phase": "Running", "ready": "True", "deleting": "", "imageID": reviewedFluxImageID}},
	}
}
func TestWriterBarrierSurvivesClosedSourceReadinessButNotChangedAuthority(t *testing.T) {
	raw := sourceFixture(t)
	var proof map[string]any
	if json.Unmarshal(raw, &proof) != nil {
		t.Fatal("fixture")
	}
	proof["Writer"] = writerFixture(t)
	// Removal deliberately makes the still-declared baseline fail Ready. Its
	// completed barrier is retained; only unchanged writer authority is reused.
	for _, layer := range proof["Kustomizations"].([]any) {
		layer.(map[string]any)["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	}
	body, _ := json.Marshal(proof)
	if err := writerCurrentProof(body); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"source-uid", "source-generation", "source-digest", "source-unverified", "layer-uid", "layer-generation", "disabled-layer", "producer-alias"} {
		var p map[string]any
		json.Unmarshal(body, &p)
		src := p["OCI"].(map[string]any)
		layers := p["Kustomizations"].([]any)
		layer := layers[0].(map[string]any)
		switch kind {
		case "source-uid":
			src["metadata"].(map[string]any)["uid"] = "replacement"
		case "source-generation":
			src["metadata"].(map[string]any)["generation"] = 3
		case "source-digest":
			src["status"].(map[string]any)["artifact"].(map[string]any)["digest"] = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		case "source-unverified":
			src["status"].(map[string]any)["conditions"] = []any{}
		case "layer-uid":
			layer["metadata"].(map[string]any)["uid"] = "replacement"
		case "layer-generation":
			layer["metadata"].(map[string]any)["generation"] = 3
		case "disabled-layer":
			layer["metadata"].(map[string]any)["annotations"] = map[string]any{reconcileKey: "disabled"}
		case "producer-alias":
			owner := p["Writer"].(map[string]any)["owner"].(map[string]any)
			owner["Run"] = owner["run"]
			delete(owner, "run")
		}
		mutation, _ := json.Marshal(p)
		if writerCurrentProof(mutation) == nil {
			t.Fatal("accepted", kind)
		}
	}
}
func TestWriterReceiptRequiresCompleteFreshBarrierBeforeDrain(t *testing.T) {
	s := baselineFixture(t)
	p, err := claim(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	s.DrainProven = true
	if _, err := advance(s, "drained"); err == nil {
		t.Fatal("drained without writer barrier")
	}
	raw := sourceFixture(t)
	var proof map[string]any
	json.Unmarshal(raw, &proof)
	proof["Ticket"] = "native-123-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, layer := range proof["Kustomizations"].([]any) {
		layer.(map[string]any)["metadata"].(map[string]any)["annotations"] = map[string]any{"reconcile.fluxcd.io/requestedAt": proof["Ticket"]}
		layer.(map[string]any)["status"].(map[string]any)["lastHandledReconcileAt"] = proof["Ticket"]
	}
	body, _ := json.Marshal(proof)
	p, err = recordWriter(s, body)
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	if _, err := advance(s, "drained"); err != nil {
		t.Fatal(err)
	}
	j, _ := readJournal(s)
	closeBaseline(&j)
	if j.Baseline.Writer == nil || j.Baseline.Writer.Owner != s.Owner {
		t.Fatal("late binding lost completed barrier")
	}
	proof["Ticket"] = "native-999-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body, _ = json.Marshal(proof)
	if _, err := recordWriter(s, body); err == nil {
		t.Fatal("foreign nonce accepted")
	}
}

func TestOpeningWriterBarrierRequiresTheCompletedExactMissingCreateDenial(t *testing.T) {
	for _, missing := range []string{"hr", "eso"} {
		t.Run(missing, func(t *testing.T) {
			s := baselineFixture(t)
			if missing == "hr" {
				s.Release = nil
			} else {
				s.Credential = nil
			}
			p, err := claim(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			applyRecord(t, &s, p)
			j, _ := readJournal(s)
			var proof map[string]any
			json.Unmarshal(sourceFixture(t), &proof)
			proof["Digest"] = j.Baseline.Digest
			proof["Ticket"] = "native-123-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			proof["FluxBefore"], proof["Flux"] = fluxFixture(), fluxFixture()
			proof["OCI"].(map[string]any)["status"].(map[string]any)["artifact"].(map[string]any)["digest"] = proof["Digest"]
			for _, value := range proof["Kustomizations"].([]any) {
				layer := value.(map[string]any)
				layer["metadata"].(map[string]any)["annotations"] = map[string]any{"reconcile.fluxcd.io/requestedAt": proof["Ticket"]}
				status := layer["status"].(map[string]any)
				status["lastHandledReconcileAt"] = proof["Ticket"]
				status["lastAppliedRevision"] = "latest@" + j.Baseline.Digest
				status["lastAttemptedRevision"] = "latest@" + j.Baseline.Digest
				if layer["metadata"].(map[string]any)["name"] == "infrastructure" {
					target, rule := "HelmRelease/arc-runners/platform-runners", "fence-pool-retirement"
					if missing == "eso" {
						target, rule = "ExternalSecret/arc-runners/arc-github-app", "fence-credential-recreation"
					}
					status["observedGeneration"] = 1 // Flux advances this field only on success.
					status["lastAppliedRevision"] = "latest@sha256:" + strings.Repeat("b", 64)
					status["conditions"] = []any{
						map[string]any{"type": "Ready", "status": "False", "reason": "ReconciliationFailed", "observedGeneration": 2, "message": target + " dry-run failed: admission webhook \"validate.kyverno.svc-fail\" denied the request:\nrestrict-arc-retirement:\n  " + rule + ": closed"},
						map[string]any{"type": "Reconciling", "status": "True", "reason": "ProgressingWithRetry", "observedGeneration": 2},
					}
				}
			}
			body, _ := json.Marshal(proof)
			if sourceProof(body) == nil {
				t.Fatal("failed opening passed the unchanged final Ready proof")
			}
			if _, err := recordWriter(s, body); err != nil {
				t.Fatal("completed missing-create denial must permit retirement:", err)
			}
			for _, kind := range []string{"generic-error", "wrong-policy", "wrong-target", "active-apply", "stale-condition", "unacknowledged", "different-digest", "other-layer-failed", "runtime-version", "runtime-rollout", "runtime-old-pod", "runtime-replaced", "duplicate-condition", "stalled", "bound-missing-identity"} {
				var mutated map[string]any
				json.Unmarshal(body, &mutated)
				layers := mutated["Kustomizations"].([]any)
				layer := layers[2].(map[string]any)
				status := layer["status"].(map[string]any)
				conditions := status["conditions"].([]any)
				ready, retry := conditions[0].(map[string]any), conditions[1].(map[string]any)
				switch kind {
				case "generic-error":
					ready["message"] = "failed to build kube client"
				case "wrong-policy":
					ready["message"] = strings.ReplaceAll(ready["message"].(string), "restrict-arc-retirement", "another-policy")
				case "wrong-target":
					ready["message"] = strings.ReplaceAll(ready["message"].(string), "arc-runners/", "another-namespace/")
				case "active-apply":
					retry["reason"] = "Progressing"
				case "stale-condition":
					ready["observedGeneration"] = 1
				case "unacknowledged":
					status["lastHandledReconcileAt"] = "old"
				case "different-digest":
					mutated["Digest"] = "sha256:" + strings.Repeat("d", 64)
				case "other-layer-failed":
					layers[0].(map[string]any)["status"].(map[string]any)["conditions"] = conditions
				case "runtime-version":
					mutated["Flux"].(map[string]any)["Pods"].([]any)[0].(map[string]any)["imageID"] = "unreviewed"
				case "runtime-rollout":
					mutated["Flux"].(map[string]any)["Deployment"].(map[string]any)["status"].(map[string]any)["updatedReplicas"] = 1
				case "runtime-old-pod":
					mutated["Flux"].(map[string]any)["Pods"].([]any)[0].(map[string]any)["deleting"] = "now"
				case "runtime-replaced":
					mutated["Flux"].(map[string]any)["Pods"].([]any)[0].(map[string]any)["uid"] = "another-process"
				case "duplicate-condition":
					status["conditions"] = append(conditions, ready)
				case "stalled":
					status["conditions"] = append(conditions, map[string]any{"type": "Stalled", "status": "True"})
				case "bound-missing-identity":
					if missing == "hr" {
						j.HRUID = "previous-hr"
						j.Baseline.HRUID = j.HRUID
					} else {
						j.ESOUID = "previous-eso"
						j.Baseline.ESOUID = j.ESOUID
					}
					patch, _ := record(s, j)
					applyRecord(t, &s, patch)
				}
				raw, _ := json.Marshal(mutated)
				if _, err := recordWriter(s, raw); err == nil {
					t.Fatal("accepted", kind)
				}
			}
		})
	}
}

func TestCompletedFailureReceiptRejectsChangedRuntimeAtDelete(t *testing.T) {
	var proof map[string]any
	json.Unmarshal(sourceFixture(t), &proof)
	flux := fluxFixture()
	proof["Flux"] = flux
	raw, _ := json.Marshal(flux)
	parsed, _ := evidence(raw)
	w := writerFixture(t)
	var err error
	w.Flux, err = fluxWriterProof(parsed)
	if err != nil {
		t.Fatal(err)
	}
	proof["Writer"] = w
	body, _ := json.Marshal(proof)
	if err := writerCurrentProof(body); err != nil {
		t.Fatal(err)
	}
	flux["Pods"].([]any)[0].(map[string]any)["uid"] = "replacement"
	body, _ = json.Marshal(proof)
	if writerCurrentProof(body) == nil {
		t.Fatal("reused completed failure after runtime replacement")
	}
}
