package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestRetirementRejectsStaleReadyRootInventory catches Ready at an older
// publication, which does not prove the current source excludes retained claims.
func TestRetirementRejectsStaleReadyRootInventory(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	layer, err := decode(encode(t, item(f.layers, 0)))
	if err != nil {
		t.Fatal(err)
	}
	object(layer["metadata"])["name"], object(layer["metadata"])["uid"] = "infrastructure", "infrastructure-uid"
	object(layer["status"])["lastAppliedRevision"] = "latest@sha256:" + strings.Repeat("c", 64)
	f.layers["items"] = append(array(f.layers["items"]), layer)
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
		t.Fatalf("stale Ready inventory authorized lifecycle writes: code=%d output=%s", code, &out)
	}
}

// TestRetirementBindsIndependentInventorySource rejects stale or unreadable
// tenant inventories without requiring unrelated products to share one digest.
func TestRetirementBindsIndependentInventorySource(t *testing.T) {
	for _, kind := range []string{"OCIRepository", "GitRepository"} {
		for _, state := range []string{"current", "stale", "missing", "replaced", "unready"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				layer, err := decode(encode(t, item(f.layers, 0)))
				if err != nil {
					t.Fatal(err)
				}
				object(layer["metadata"])["namespace"], object(layer["metadata"])["name"], object(layer["metadata"])["uid"] = "tenant", "tenant", "tenant-layer-uid"
				layer["spec"] = map[string]any{"sourceRef": map[string]any{"kind": kind, "name": "tenant-source", "namespace": "tenant-sources"}}
				object(layer["status"])["lastAppliedRevision"] = "tenant-publication"
				object(field(layer, "status", "inventory"))["entries"] = []any{map[string]any{"id": "tenant_example__ConfigMap"}}
				f.layers["items"] = append(array(f.layers["items"]), layer)
				source, err := decode(encode(t, f.sources))
				if err != nil {
					t.Fatal(err)
				}
				source["kind"] = kind
				object(source["metadata"])["namespace"], object(source["metadata"])["name"], object(source["metadata"])["uid"] = "tenant-sources", "tenant-source", "tenant-source-uid"
				object(source["status"])["artifact"] = map[string]any{"revision": "tenant-publication"}
				switch state {
				case "stale":
					object(source["status"])["artifact"] = map[string]any{"revision": "new-tenant-publication"}
				case "missing":
					source = nil
				case "replaced":
					object(source["metadata"])["name"] = "other-source"
				case "unready":
					object(source["status"])["observedGeneration"] = 0
				}
				reader := func(ctx context.Context, name string, args ...string) ([]byte, error) {
					want := []string{"--context=admin@prod", "--request-timeout=30s", "--namespace=tenant-sources", "get", strings.ToLower(kind), "tenant-source", "--output=json"}
					if name == "kubectl" && reflect.DeepEqual(args, want) {
						return encode(t, source), nil
					}
					return f.execute(ctx, name, args...)
				}
				var out bytes.Buffer
				code := run([]string{"--execute"}, &out, reader)
				if state == "current" {
					if code != 0 || !f.deleted {
						t.Fatalf("independent current source prevented retirement: code=%d output=%s", code, &out)
					}
				} else if code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
					t.Fatalf("unverified inventory source authorized lifecycle writes: code=%d output=%s", code, &out)
				}
			})
		}
	}
}

// TestRetirementRejectsInventorySourceReplacementBetweenSamples prevents a
// same-name replacement from becoming a fresh inventory witness mid-preflight.
func TestRetirementRejectsInventorySourceReplacementBetweenSamples(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	reads := 0
	reader := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "kubectl" && strings.Contains(strings.Join(args, " "), "get ocirepository flux-system") {
			reads++
			if reads == 2 {
				object(f.sources["metadata"])["uid"] = "replacement-source-uid"
			}
		}
		return f.execute(ctx, name, args...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, reader); code != 2 || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
		t.Fatalf("replaced inventory source authorized lifecycle writes: code=%d output=%s", code, &out)
	}
}
