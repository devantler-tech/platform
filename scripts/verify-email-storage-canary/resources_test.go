package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanaryHasNoOperationalKeyOrMailboxReference(t *testing.T) {
	ns, run := "email-storage-canary-123", "123"
	sc := storageClassManifest(ns, ns, run)
	if sc["provisioner"] != "driver.longhorn.io" || sc["reclaimPolicy"] != "Delete" || child(sc, "parameters")["encrypted"] != "true" {
		t.Fatal("synthetic volume lost encrypted provisioning and explicit disposable lifecycle")
	}
	for _, stage := range []string{"stage", "publish", "expand"} {
		p := child(sc, "parameters")
		if p["csi.storage.k8s.io/node-"+stage+"-secret-namespace"] != ns || p["csi.storage.k8s.io/node-"+stage+"-secret-name"] != "encryption-key" {
			t.Fatal("canary key reference escapes its own namespace")
		}
	}
	for _, o := range []object{namespaceManifest(ns, run), policyManifest(ns, run), sc, keyManifest(ns, run, "synthetic-not-an-operational-key"), claimManifest(ns, ns, run), podManifest(ns, "write", run)} {
		encoded, _ := json.Marshal(o)
		if strings.Contains(string(encoded), "private-mail") || strings.Contains(string(encoded), "infrastructure/email") || strings.Contains(string(encoded), "email-storage-key") {
			t.Fatal("canary can reach operational Email custody")
		}
	}
	pod := podManifest(ns, "read", run)
	spec := child(pod, "spec")
	if spec["automountServiceAccountToken"] != false || child(spec, "securityContext")["runAsNonRoot"] != true || len(items(spec, "containers")) != 1 || len(items(spec, "volumes")) != 1 {
		t.Fatal("canary pod gained an unneeded access path")
	}
	if text(items(spec, "containers")[0], "image") != probeImage {
		t.Fatal("probe image is not pinned")
	}
}

func TestProbeDefinitionRefusesAdmissionChangesThatCouldFakeTheProof(t *testing.T) {
	expected := podManifest("email-storage-canary-123", "read", "123")
	// Normalize to the representation returned by Kubernetes JSON.
	raw, _ := json.Marshal(expected)
	var actual object
	_ = json.Unmarshal(raw, &actual)
	if !sameProbeSpec(actual, expected) {
		t.Fatal("unaltered probe refused")
	}
	for _, mutate := range []func(object){
		func(o object) { items(child(o, "spec"), "containers")[0]["command"] = []any{"/bin/true"} },
		func(o object) { child(o, "spec")["automountServiceAccountToken"] = true },
		func(o object) {
			items(child(o, "spec"), "volumes")[0]["persistentVolumeClaim"] = object{"claimName": "another-claim"}
		},
		func(o object) {
			items(child(o, "spec"), "containers")[0]["env"] = []any{object{"name": "PRIVATE", "valueFrom": object{"secretKeyRef": object{"name": "other"}}}}
		},
	} {
		var changed object
		_ = json.Unmarshal(raw, &changed)
		mutate(changed)
		if sameProbeSpec(changed, expected) {
			t.Fatal("changed probe could supply a false receipt or added access")
		}
	}
}
