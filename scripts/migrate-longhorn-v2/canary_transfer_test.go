package main

import "testing"

func transferFixture(kind string) (object, object, object) {
	parent, pod, transfer := resource("canary"), resource("fio"), resource("transfer")
	parent["kind"] = kind
	pod["metadata"].(object)["namespace"] = "longhorn-canary-v1-1234"
	transfer["metadata"].(object)["namespace"] = "velero"
	transfer["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "velero.io/v1", "kind": kind, "name": text(parent, "metadata", "name"), "uid": text(parent, "metadata", "uid"), "controller": true}}
	// Velero leaves the Restic-only repoIdentifier field absent for Kopia.
	transfer["spec"] = object{"pod": object{"name": "fio", "namespace": text(pod, "metadata", "namespace"), "uid": text(pod, "metadata", "uid")}, "volume": "data", "backupStorageLocation": "default", "uploaderType": "kopia", "snapshotID": "snapshot", "sourceNamespace": "longhorn-canary-v1-1234"}
	transfer["status"] = object{"phase": "Completed", "completionTimestamp": "2026-10-29T19:00:00Z", "snapshotID": "snapshot", "progress": object{"bytesDone": float64(134217800), "totalBytes": float64(134217800)}}
	return parent, pod, transfer
}

func TestCanaryRequiresTheExactCompletedDataTransfer(t *testing.T) {
	for _, kind := range []string{"Backup", "Restore"} {
		parent, pod, transfer := transferFixture(kind)
		if _, err := verifyCanaryTransfer([]object{transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err != nil {
			t.Fatal(err)
		}
		for name, mutate := range map[string]func(object){
			"unrelated parent":        func(o object) { o["metadata"].(object)["ownerReferences"] = []any{} },
			"wrong pod uid":           func(o object) { o["spec"].(object)["pod"].(object)["uid"] = "replacement" },
			"wrong volume":            func(o object) { o["spec"].(object)["volume"] = "scratch" },
			"wrong backup location":   func(o object) { o["spec"].(object)["backupStorageLocation"] = "other" },
			"unproven progress":       func(o object) { o["status"].(object)["progress"] = object{} },
			"incomplete transfer":     func(o object) { o["status"].(object)["progress"].(object)["bytesDone"] = float64(32) },
			"failed transfer":         func(o object) { o["status"].(object)["phase"] = "Failed" },
			"restic instead of kopia": func(o object) { o["spec"].(object)["uploaderType"] = "restic" },
			"restic repository id":    func(o object) { o["spec"].(object)["repoIdentifier"] = "restic-only" },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				p, pod, transfer := transferFixture(kind)
				mutate(transfer)
				if _, err := verifyCanaryTransfer([]object{transfer}, p, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
					t.Fatal("backup metadata was mistaken for actual volume-data recovery")
				}
			})
		}
		if _, err := verifyCanaryTransfer([]object{transfer, transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
			t.Fatal("ambiguous transfer inventory accepted")
		}
	}
	p, pod, transfer := transferFixture("Restore")
	transfer["spec"].(object)["snapshotID"] = "older-backup"
	if _, err := verifyCanaryTransfer([]object{transfer}, p, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
		t.Fatal("restore from a different snapshot accepted")
	}
}
