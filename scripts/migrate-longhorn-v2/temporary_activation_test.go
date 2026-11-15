package main

import (
	"testing"
)

// temporaryRawBindingFixture supplies one pinned worker and separate encrypted
// filesystem and raw partitions without using the registration builder.
func temporaryRawBindingFixture() (object, object, object, object, migrationPool) {
	n := capacityFixture()[3]
	n["spec"] = object{"providerID": "hcloud://123", "unschedulable": false}
	n["status"].(object)["nodeInfo"] = object{"systemUUID": "hardware", "bootID": "12345678-1234-4234-8234-000000000001", "osImage": "Talos fixture", "architecture": "amd64"}
	n["status"].(object)["allocatable"].(object)["cpu"] = "8"
	lh := resource(text(n, "metadata", "name"))
	lh["metadata"].(object)["namespace"] = "longhorn-system"
	raw, eph := rawFixture()
	valueObject(raw, "spec")["size"] = float64(144 << 30)
	raw["spec"].(object)["partitionUUID"] = "12345678-1234-4234-8234-000000000002"
	return n, lh, raw, eph, migrationPool{name: "autoscale-longhorn-v1", role: "v1", serverType: "cpx42", count: 2}
}

// Wrong ownership, boot, encryption, capacity or pool selection must prevent a
// native partition observation from becoming registration evidence.
func TestTemporaryRawRegistrationBindsTheNativePartitionsAndPoolMember(t *testing.T) {
	n, lh, raw, eph, pool := temporaryRawBindingFixture()
	binding, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID"))
	if err != nil || binding.Name != text(n, "metadata", "name") || binding.RawPartitionUUID != text(raw, "spec", "partitionUUID") || binding.EphemeralUUID != text(eph, "spec", "uuid") {
		t.Fatal("qualified temporary raw storage was not bound to its native identities:", err)
	}
	for name, change := range map[string]func(object, object, object, object){
		"foreign storage identity":           func(_ object, lh object, _, _ object) { lh["metadata"].(object)["name"] = "other" },
		"missing raw identity":               func(_, _ object, raw, _ object) { delete(raw["spec"].(object), "partitionUUID") },
		"raw and filesystem identity reused": func(_, _ object, raw, eph object) { raw["spec"].(object)["partitionUUID"] = text(eph, "spec", "uuid") },
		"unencrypted raw disk":               func(_, _ object, raw, _ object) { raw["spec"].(object)["encryptionProvider"] = "" },
		"original smaller raw capacity":      func(_, _ object, raw, _ object) { raw["spec"].(object)["size"] = float64(96 << 30) },
		"cordoned member":                    func(n, _, _, _ object) { n["spec"].(object)["unschedulable"] = true },
		"unowned capacity": func(n, _, _, _ object) {
			delete(n["metadata"].(object)["annotations"].(object), capacityOwnerAnnotation)
		},
		"automatic disk creation enabled": func(n, _, _, _ object) {
			n["metadata"].(object)["labels"].(object)["node.longhorn.io/create-default-disk"] = "config"
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, lh, raw, eph, pool := temporaryRawBindingFixture()
			change(n, lh, raw, eph)
			if _, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID")); err == nil {
				t.Fatal("unproven temporary registration binding was accepted")
			}
		})
	}
	if _, err := projectTemporaryV2Binding("temporary-v1-1", n, lh, raw, eph, pool, "12345678-1234-4234-8234-000000000099"); err == nil {
		t.Fatal("a native boot mismatch was accepted")
	}
	if _, err := projectTemporaryV2Binding("temporary-v2-1", n, lh, raw, eph, pool, text(n, "status", "nodeInfo", "bootID")); err == nil {
		t.Fatal("a selection from another pool was accepted")
	}
}

// Resumption recognizes only an empty fence or this journal's exact raw disk;
// matching paths without ownership never authorize adoption.
func TestTemporaryRawRegistrationAcceptsOnlyItsOwnPartialActivation(t *testing.T) {
	storage := func(enabled bool) object {
		_, lh, _, _, _ := temporaryRawBindingFixture()
		lh["metadata"].(object)["annotations"] = object{}
		lh["metadata"].(object)["labels"] = object{}
		lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
		lh["status"] = object{"diskStatus": object{}}
		if enabled {
			lh["metadata"].(object)["annotations"].(object)[temporaryActivationOwner] = "journal-uid"
			lh["metadata"].(object)["labels"].(object)[migrationEnabledLabel] = "true"
			lh["spec"] = object{"allowScheduling": true, "evictionRequested": false, "disks": blockDisks()}
		}
		return lh
	}
	if registered, err := temporaryRegistrationState(storage(false), "journal-uid", "temporary-v1-1"); err != nil || registered {
		t.Fatal("empty fenced storage could not be registered:", err)
	}
	if registered, err := temporaryRegistrationState(storage(true), "journal-uid", "temporary-v1-1"); err != nil || !registered {
		t.Fatal("the journal's exact existing activation could not resume:", err)
	}
	if _, err := temporaryRegistrationState(storage(true), "journal-uid", "temporary-v2-1"); err == nil {
		t.Fatal("the retained mapper qualified as staged storage")
	}
	for name, change := range map[string]func(object){
		"foreign journal":   func(lh object) { lh["metadata"].(object)["annotations"].(object)[temporaryActivationOwner] = "foreign" },
		"missing ownership": func(lh object) { delete(lh["metadata"].(object)["annotations"].(object), temporaryActivationOwner) },
		"filesystem substitution": func(lh object) {
			lh["spec"].(object)["disks"] = object{"filesystem": object{"path": "/var/lib/longhorn"}}
		},
		"unknown eviction state": func(lh object) { delete(lh["spec"].(object), "evictionRequested") },
	} {
		t.Run(name, func(t *testing.T) {
			lh := storage(true)
			change(lh)
			if _, err := temporaryRegistrationState(lh, "journal-uid", "temporary-v1-1"); err == nil {
				t.Fatal("a foreign or changed disk registration was adopted")
			}
		})
	}
	if _, err := temporaryRegistrationState(storage(true), "", "temporary-v1-1"); err == nil {
		t.Fatal("an unjournaled existing registration was adopted")
	}
}
