package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRetirementPreservesControllerClaimWithoutFluxPruneOptOut(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	claim := item(f.live, 3)
	delete(object(claim["metadata"]), "annotations")
	before := encode(t, claim)
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 0 || !f.deleted || !f.fenceInstalled {
		t.Fatalf("retained controller claim prevented stateless retirement: code=%d output=%s", code, &out)
	}
	if !bytes.Equal(before, encode(t, claim)) || field(item(f.volumes, 0), "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		t.Fatal("retirement changed the retained claim or physical retention")
	}
}

// A controller claim outside Flux inventory is still unsafe until its exact
// bound volume has physical retention. This must stop before even the journal.
func TestControllerClaimWithoutPruneOptOutRequiresPhysicalRetention(t *testing.T) {
	for _, policy := range []any{"Delete", "Recycle", "", nil} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			claim := item(f.live, 3)
			delete(object(claim["metadata"]), "annotations")
			object(item(f.volumes, 0)["spec"])["persistentVolumeReclaimPolicy"] = policy
			before := encode(t, claim)
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
				t.Fatalf("unretained controller claim admitted lifecycle writes: policy=%v code=%d output=%s", policy, code, &out)
			}
			if !bytes.Equal(before, encode(t, claim)) {
				t.Fatal("failed retention precondition changed claim metadata")
			}
		})
	}
}

// Flux transcodes RBAC name colons as double underscores. These unrelated,
// valid entries must not prevent proving the controller claim is unmanaged.
func TestRetirementAcceptsEscapedRBACInventoryNames(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	delete(object(item(f.live, 3)["metadata"]), "annotations")
	object(field(item(f.layers, 0), "status", "inventory"))["entries"] = []any{
		map[string]any{"id": "_backstage__Namespace"},
		map[string]any{"id": "_example__reader__events_rbac.authorization.k8s.io_ClusterRole"},
		map[string]any{"id": "system_system____reader_rbac.authorization.k8s.io_Role"},
		map[string]any{"id": "system_example_reader_rbac.authorization.k8s.io_Role"},
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 0 || !f.deleted || !f.fenceInstalled {
		t.Fatalf("valid escaped RBAC inventory prevented retirement: code=%d output=%s", code, &out)
	}
}

func TestRetirementRejectsAmbiguousClaimOwnershipBeforeLifecycleWrites(t *testing.T) {
	for name, change := range map[string]func(*portalFixture){
		"unlabelled claim": func(f *portalFixture) { delete(object(item(f.live, 3)["metadata"]), "labels") },
		"foreign cluster label": func(f *portalFixture) {
			object(field(item(f.live, 3), "metadata", "labels"))["cnpg.io/cluster"] = "another-db"
		},
		"missing owner": func(f *portalFixture) { delete(object(item(f.live, 3)["metadata"]), "ownerReferences") },
		"replaced owner": func(f *portalFixture) {
			object(array(field(item(f.live, 3), "metadata", "ownerReferences"))[0])["uid"] = "replacement"
		},
		"foreign owner api": func(f *portalFixture) {
			object(array(field(item(f.live, 3), "metadata", "ownerReferences"))[0])["apiVersion"] = "other.example/v1"
		},
		"non-controller owner": func(f *portalFixture) {
			object(array(field(item(f.live, 3), "metadata", "ownerReferences"))[0])["controller"] = false
		},
		"foreign claim name": func(f *portalFixture) { object(item(f.live, 3)["metadata"])["name"] = "foreign-claim" },
		"flux owned claim": func(f *portalFixture) {
			object(field(item(f.layers, 0), "status", "inventory"))["entries"] = []any{map[string]any{"id": "backstage_backstage-db-3__PersistentVolumeClaim"}}
		},
		"missing inventory":      func(f *portalFixture) { delete(object(item(f.layers, 0)["status"]), "inventory") },
		"foreign inventory kind": func(f *portalFixture) { item(f.layers, 0)["kind"] = "ConfigMap" },
		"foreign inventory api":  func(f *portalFixture) { item(f.layers, 0)["apiVersion"] = "other.example/v1" },
		"malformed inventory identity": func(f *portalFixture) {
			object(field(item(f.layers, 0), "status", "inventory"))["entries"] = []any{map[string]any{"id": "not-an-inventory-identity"}}
		},
		"missing inventory name": func(f *portalFixture) {
			object(field(item(f.layers, 0), "status", "inventory"))["entries"] = []any{map[string]any{"id": "__rbac.authorization.k8s.io_ClusterRole"}}
		},
		"incomplete layer list": func(f *portalFixture) { f.layers["metadata"] = map[string]any{"continue": "next-page"} },
	} {
		for _, prune := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/without-opt-out", true: "/with-opt-out"}[prune], func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				if !prune {
					delete(object(item(f.live, 3)["metadata"]), "annotations")
				}
				change(f)
				var out bytes.Buffer
				if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
					t.Fatalf("ambiguous claim admitted lifecycle writes: code=%d output=%s", code, &out)
				}
			})
		}
	}
}

func TestRetirementRejectsClaimInOtherFluxNamespace(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	other, err := decode(encode(t, item(f.layers, 0)))
	if err != nil {
		t.Fatal(err)
	}
	object(other["metadata"])["namespace"] = "another-system"
	object(other["metadata"])["uid"] = "other-layer"
	object(field(other, "status", "inventory"))["entries"] = []any{map[string]any{"id": "backstage_backstage-db-3__PersistentVolumeClaim"}}
	f.layers["items"] = append(array(f.layers["items"]), other)
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.deleted {
		t.Fatalf("another Flux namespace could prune the claim: code=%d output=%s", code, &out)
	}
}

func TestRetirementRejectsClaimInventoryDriftBetweenOpeningSamples(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	reads := 0
	execute := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "kubectl" && strings.Contains(strings.Join(args, " "), "get kustomizations.kustomize.toolkit.fluxcd.io") {
			reads++
			if reads == 2 {
				object(field(item(f.layers, 0), "status", "inventory"))["entries"] = []any{map[string]any{"id": "backstage_backstage-db-3__PersistentVolumeClaim"}}
			}
		}
		return f.execute(ctx, name, args...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 2 || reads != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
		t.Fatalf("new Flux claim ownership admitted lifecycle writes: code=%d reads=%d output=%s", code, reads, &out)
	}
}

func TestRetirementRequiresNamespaceProtectionWithoutClaimOptOut(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	delete(object(item(f.live, 3)["metadata"]), "annotations")
	delete(object(f.namespace["metadata"]), "annotations")
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.deleted {
		t.Fatalf("unprotected namespace admitted lifecycle writes: code=%d output=%s", code, &out)
	}
}

func TestRetainedClaimProjectionPreservesUnrelatedAnnotations(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	claim := item(f.live, 3)
	object(claim["metadata"])["annotations"] = map[string]any{"example.org/retained": "unchanged"}
	before := encode(t, claim)
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 0 || !bytes.Equal(before, encode(t, claim)) {
		t.Fatalf("controller claim metadata changed or was refused: code=%d output=%s", code, &out)
	}
}
