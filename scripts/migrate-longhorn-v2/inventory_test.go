package main

import (
	"encoding/json"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

func inventoryFixture() (object, object, []object) {
	node := nodeFixture()
	node["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-123456789abc"
	lh := resource("prod-worker-1")
	lh["spec"] = object{"disks": object{"slot": object{"path": "/var/lib/longhorn", "diskType": "filesystem", "diskDriver": ""}}}
	lh["status"] = object{"diskStatus": object{"slot": object{"diskUUID": "disk-uuid", "diskPath": "/var/lib/longhorn", "diskName": "native-name", "diskType": "filesystem", "diskDriver": ""}}}
	r := resource("historical")
	r["metadata"].(object)["deletionTimestamp"] = "2026-11-01T00:00:00Z"
	r["spec"] = object{"nodeID": "prod-worker-1", "diskID": "disk-uuid", "diskPath": "/var/lib/longhorn", "dataDirectoryName": "old-data", "dataEngine": "v1", "active": false, "failedAt": "2026-06-16T00:00:00Z", "volumeName": "missing-volume"}
	r["status"] = object{"currentState": "stopped"}
	return node, lh, []object{r}
}

func TestInventoryPreservesTerminatingFailedAndVolumeLessOwners(t *testing.T) {
	n, lh, replicas := inventoryFixture()
	projection, err := projectInventory("prod-worker-1", n, lh, replicas)
	if err != nil || len(projection.Replicas) != 1 || projection.Replicas[0].Directory != "old-data" || projection.Address != "10.0.99.4" || projection.BootID == "" || projection.Digest == "" {
		t.Fatalf("historical ownership was silently filtered: %v", err)
	}
	listing := longhorninventory.Listing{Complete: true, Entries: []longhorninventory.Entry{{Name: ".", Mode: 1 << 31, IsDir: true}, {Name: "unreported", Mode: 1 << 31, IsDir: true}}}
	report, err := longhorninventory.Analyze(listing, projection.Replicas)
	if err != nil || report.Unmatched != 1 || report.Missing != 1 || report.EmptyAndAccounted {
		t.Fatal("unreported physical data and missing API data were not both retained")
	}
}

func TestInventoryProjectionRejectsAmbiguousIdentityAndPlacement(t *testing.T) {
	for name, change := range map[string]func(object, object, []object){
		"missing boot":         func(n, _ object, _ []object) { delete(n["status"].(object)["nodeInfo"].(object), "bootID") },
		"different node":       func(n, _ object, _ []object) { n["metadata"].(object)["name"] = "prod-worker-2" },
		"missing disk binding": func(_ object, lh object, _ []object) { delete(lh["status"].(object), "diskStatus") },
		"foreign disk UUID":    func(_, _ object, r []object) { r[0]["spec"].(object)["diskID"] = "other" },
		"escaping directory":   func(_, _ object, r []object) { r[0]["spec"].(object)["dataDirectoryName"] = "../other" },
		"unknown placement":    func(_, _ object, r []object) { r[0]["spec"].(object)["nodeID"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			n, lh, r := inventoryFixture()
			change(n, lh, r)
			if _, err := projectInventory("prod-worker-1", n, lh, r); err == nil {
				t.Fatal("unresolved placement was accepted as complete")
			}
		})
	}
}

func TestInventoryCollectionRequiresCompleteBoundedJSONAndObjectReceipts(t *testing.T) {
	n, _, _ := inventoryFixture()
	valid := object{"metadata": object{"resourceVersion": "123"}, "items": []any{n}}
	for name, change := range map[string]func(object){
		"missing collection version": func(v object) { delete(v["metadata"].(object), "resourceVersion") },
		"unread page":                func(v object) { v["metadata"].(object)["continue"] = "next" },
		"missing object version":     func(v object) { delete(v["items"].([]any)[0].(object)["metadata"].(object), "resourceVersion") },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var v object
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatal(err)
			}
			change(v)
			data, _ = json.Marshal(v)
			if _, err := decodeInventory(data); err == nil {
				t.Fatal("incomplete inventory accepted")
			}
		})
	}
	data, _ := json.Marshal(valid)
	if _, err := decodeInventory(data); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeInventory([]byte(`{"metadata":{"resourceVersion":"1"},"items":[],"items":[]}`)); err == nil {
		t.Fatal("duplicate evidence accepted")
	}
}

func TestInventoryAuthorizationCannotReachMutationOperations(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "inventory-replicas"
	env["LONGHORN_V2_CONFIRM"] = "inventory-longhorn-replicas-prod-worker-1"
	getenv := func(k string) string { return env[k] }
	if err := authorizeInventory(getenv, "prod-worker-1"); err != nil {
		t.Fatal(err)
	}
	if authorize(getenv, "prod-worker-1") == nil || authorizeRefresh(getenv) == nil || authorizeBenchmark(getenv, "v1") == nil {
		t.Fatal("read-only inventory authorized a mutation")
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizeInventory(getenv, "prod-worker-1") == nil {
		t.Fatal("inventory bypassed first-attempt source authorization")
	}
}
