package portalstorage

import (
	"encoding/json"
	"reflect"
	"testing"
)

// bindingFixture supplies independent literal reciprocal bindings with mixed retention policies.
func bindingFixture(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	const claims = `{"apiVersion":"v1","kind":"List","metadata":{"resourceVersion":"100"},"items":[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"backstage-db-3","namespace":"backstage","uid":"claim-3","resourceVersion":"101","ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"volumeName":"pv-3","storageClassName":"longhorn-wffc"},"status":{"phase":"Bound"}},{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"backstage-db-4","namespace":"backstage","uid":"claim-4","resourceVersion":"102","ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"volumeName":"pv-4","storageClassName":"longhorn-wffc"},"status":{"phase":"Bound"}}]}`
	const volumes = `{"apiVersion":"v1","kind":"List","metadata":{"resourceVersion":"200"},"items":[{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-4","uid":"volume-4","resourceVersion":"202"},"spec":{"persistentVolumeReclaimPolicy":"Delete","storageClassName":"longhorn-wffc","claimRef":{"apiVersion":"v1","kind":"PersistentVolumeClaim","namespace":"backstage","name":"backstage-db-4","uid":"claim-4"},"csi":{"driver":"driver.longhorn.io","volumeHandle":"disk-4"}},"status":{"phase":"Bound"}},{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-3","uid":"volume-3","resourceVersion":"201"},"spec":{"persistentVolumeReclaimPolicy":"Retain","storageClassName":"longhorn-wffc","claimRef":{"apiVersion":"v1","kind":"PersistentVolumeClaim","namespace":"backstage","name":"backstage-db-3","uid":"claim-3"},"csi":{"driver":"driver.longhorn.io","volumeHandle":"disk-3"}},"status":{"phase":"Bound"}}]}`
	var c, v map[string]any
	if json.Unmarshal([]byte(claims), &c) != nil || json.Unmarshal([]byte(volumes), &v) != nil {
		t.Fatal("invalid hand-written fixture")
	}
	return c, v
}

// item selects an object from a known synthetic list.
func item(list map[string]any, n int) map[string]any {
	return list["items"].([]any)[n].(map[string]any)
}

// child selects a known synthetic nested object.
func child(m map[string]any, key string) map[string]any { return m[key].(map[string]any) }

// TestValidateBindingsJoinsEveryClaimWithoutMutation verifies stable ordered joins without changing input evidence.
func TestValidateBindingsJoinsEveryClaimWithoutMutation(t *testing.T) {
	c, v := bindingFixture(t)
	before, err := json.Marshal([]any{c, v})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ValidateBindings("database", c, v, false)
	if err != nil || len(b) != 2 {
		t.Fatalf("got %d bindings, error %v; want both claims", len(b), err)
	}
	if child(b[0].Claim, "metadata")["name"] != "backstage-db-3" || child(b[0].Volume, "metadata")["name"] != "pv-3" || child(b[1].Volume, "metadata")["name"] != "pv-4" {
		t.Fatal("wrong reciprocal binding")
	}
	after, err := json.Marshal([]any{c, v})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("validator changed evidence")
	}
}

// TestValidateBindingsRefusesUnsafeOrIncompleteEvidence exercises destructive policies, replacement, incomplete lists and extra backing identities.
func TestValidateBindingsRefusesUnsafeOrIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(map[string]any, map[string]any)
		retained bool
	}{
		{"delete is not retained", func(c, v map[string]any) {}, true},
		{"recycle is not a permitted migration", func(c, v map[string]any) { child(item(v, 0), "spec")["persistentVolumeReclaimPolicy"] = "Recycle" }, false},
		{"missing reclaim policy", func(c, v map[string]any) { delete(child(item(v, 0), "spec"), "persistentVolumeReclaimPolicy") }, false},
		{"wrong volume kind", func(c, v map[string]any) { item(v, 0)["kind"] = "PersistentVolumeClaim" }, false},
		{"wrong volume api", func(c, v map[string]any) { item(v, 0)["apiVersion"] = "example.test/v1" }, false},
		{"namespaced volume", func(c, v map[string]any) { child(item(v, 0), "metadata")["namespace"] = "backstage" }, false},
		{"claim replacement", func(c, v map[string]any) { child(item(c, 1), "metadata")["uid"] = "replacement" }, false},
		{"foreign claim reference kind", func(c, v map[string]any) { child(child(item(v, 0), "spec"), "claimRef")["kind"] = "Secret" }, false},
		{"foreign claim reference api", func(c, v map[string]any) {
			child(child(item(v, 0), "spec"), "claimRef")["apiVersion"] = "example.test/v1"
		}, false},
		{"class mismatch", func(c, v map[string]any) { child(item(v, 0), "spec")["storageClassName"] = "other" }, false},
		{"missing class", func(c, v map[string]any) { delete(child(item(c, 1), "spec"), "storageClassName") }, false},
		{"terminating claim", func(c, v map[string]any) { child(item(c, 1), "metadata")["deletionTimestamp"] = "now" }, false},
		{"terminating volume", func(c, v map[string]any) { child(item(v, 0), "metadata")["deletionTimestamp"] = "now" }, false},
		{"unbound volume", func(c, v map[string]any) { child(item(v, 0), "status")["phase"] = "Available" }, false},
		{"wrong database owner", func(c, v map[string]any) {
			child(item(c, 0), "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "other"
		}, false},
		{"partial claims page", func(c, v map[string]any) { child(c, "metadata")["continue"] = "next" }, false},
		{"partial volumes page", func(c, v map[string]any) { child(v, "metadata")["remainingItemCount"] = float64(1) }, false},
		{"extra backing volume", func(c, v map[string]any) {
			duplicate := map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "extra", "uid": "extra", "resourceVersion": "999"}, "spec": map[string]any{"claimRef": map[string]any{"namespace": "backstage", "name": "backstage-db-3", "uid": "claim-3"}}, "status": map[string]any{"phase": "Bound"}}
			v["items"] = append(v["items"].([]any), duplicate)
		}, false},
		{"duplicate claim", func(c, v map[string]any) { c["items"] = append(c["items"].([]any), item(c, 0)) }, false},
		{"empty claims", func(c, v map[string]any) { c["items"] = []any{} }, false},
		{"extra volume references retained UID from foreign namespace", func(c, v map[string]any) {
			v["items"] = append(v["items"].([]any), map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "foreign", "uid": "foreign", "resourceVersion": "999"}, "spec": map[string]any{"claimRef": map[string]any{"namespace": "other", "name": "other", "uid": "claim-3"}}})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, v := bindingFixture(t)
			tc.mutate(c, v)
			if _, err := ValidateBindings("database", c, v, tc.retained); err == nil {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
}

// TestReleasedRetainedVolumeWithRemovedClaimStillHolds prevents preserved orphan data from disappearing from the reverse census.
func TestReleasedRetainedVolumeWithRemovedClaimStillHolds(t *testing.T) {
	c, v := bindingFixture(t)
	v["items"] = append(v["items"].([]any), map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": "previous-volume", "uid": "previous-volume-uid", "resourceVersion": "999"},
		"spec": map[string]any{
			"persistentVolumeReclaimPolicy": "Retain", "storageClassName": "longhorn-wffc",
			"claimRef": map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": "backstage", "name": "backstage-db-2", "uid": "removed-claim-uid"},
			"csi":      map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "preserved-data"},
		},
		"status": map[string]any{"phase": "Released"},
	})
	before, err := json.Marshal([]any{c, v})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBindings("database", c, v, false); err == nil || err.Error() != "volume reverse census differs" {
		t.Fatalf("released retained data must stop the reverse census, got %v", err)
	}
	after, err := json.Marshal([]any{c, v})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("refusal changed retained evidence")
	}
}
