package main

import "testing"

func TestListRequiresCompleteDistinctResourceIdentities(t *testing.T) {
	good := []byte(`{"metadata":{},"items":[{"metadata":{"name":"volume","uid":"uid","resourceVersion":"1"}}]}`)
	if list, err := decodeList(good); err != nil || len(list) != 1 {
		t.Fatalf("complete list rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"no items":       `{}`,
		"null items":     `{"items":null}`,
		"invalid JSON":   `{"items":[`,
		"continuation":   `{"metadata":{"continue":"next"},"items":[]}`,
		"missing UID":    `{"items":[{"metadata":{"name":"volume"}}]}`,
		"duplicate name": `{"items":[{"metadata":{"name":"volume","uid":"a"}},{"metadata":{"name":"volume","uid":"b"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeList([]byte(raw)); err == nil {
				t.Fatal("incomplete resource inventory accepted")
			}
		})
	}
}

func rawFixture() (object, object) {
	raw := object{"metadata": object{"id": "r-longhorn"}, "spec": object{
		"phase": "ready", "type": "partition", "size": float64(96 * 1024 * 1024 * 1024),
		"location": "/dev/sda6", "mountLocation": "/dev/dm-2", "parentLocation": "/dev/sda",
		"encryptionProvider": "luks2", "configuredEncryptionKeys": []any{"nodeID"}, "encryptionLockedToState": true,
	}}
	ephemeral := object{"metadata": object{"id": "EPHEMERAL"}, "spec": object{
		"phase": "ready", "type": "partition", "size": float64(48 * 1024 * 1024 * 1024),
		"location": "/dev/sda5", "mountLocation": "/dev/dm-1", "parentLocation": "/dev/sda", "uuid": "new-ephemeral",
		"encryptionProvider": "luks2", "configuredEncryptionKeys": []any{"nodeID"}, "encryptionLockedToState": true,
	}}
	return raw, ephemeral
}

func TestResetRequiresNewEncryptedPartitionLayout(t *testing.T) {
	raw, ephemeral := rawFixture()
	if err := verifyResetLayout(raw, ephemeral, "old-ephemeral"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(object, object){
		"raw volume is not ready":            func(r, _ object) { r["spec"].(object)["phase"] = "failed" },
		"raw volume is unencrypted":          func(r, _ object) { delete(r["spec"].(object), "encryptionProvider") },
		"raw partition is not unlocked":      func(r, _ object) { r["spec"].(object)["mountLocation"] = "/dev/sda6" },
		"raw key is not state-locked":        func(r, _ object) { r["spec"].(object)["encryptionLockedToState"] = false },
		"raw partition is undersized":        func(r, _ object) { r["spec"].(object)["size"] = float64(1024) },
		"ephemeral was not rebuilt":          func(_, e object) { e["spec"].(object)["uuid"] = "old-ephemeral" },
		"ephemeral still consumes disk":      func(_, e object) { e["spec"].(object)["size"] = float64(160 * 1024 * 1024 * 1024) },
		"raw storage is on a different disk": func(r, _ object) { r["spec"].(object)["parentLocation"] = "/dev/sdb" },
		"ephemeral encryption was lost":      func(_, e object) { delete(e["spec"].(object), "encryptionProvider") },
	} {
		t.Run(name, func(t *testing.T) {
			r, e := rawFixture()
			mutate(r, e)
			if err := verifyResetLayout(r, e, "old-ephemeral"); err == nil {
				t.Fatal("unsafe rebuilt layout accepted")
			}
		})
	}
}

func TestFreshLargerMigrationWorkerRetainsExtraRootCapacity(t *testing.T) {
	r, e := rawFixture()
	e["spec"].(object)["size"] = float64(200 * 1024 * 1024 * 1024)
	if err := verifyEncryptedLocalLayout(r, e); err != nil {
		t.Fatal("fresh larger worker layout rejected:", err)
	}
	if err := verifyResetLayout(r, e, "old-root"); err == nil {
		t.Fatal("larger migration-worker geometry weakened the permanent-worker reset proof")
	}
}
