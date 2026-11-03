package main

import "testing"

func csiTransferFixture(kind string) (object, object, object, object) {
	parent := resource("longhorn-v2-canary-1234")
	parent["kind"] = kind
	parent["metadata"].(object)["namespace"] = "velero"
	source, target := resource("fio-data"), resource("fio-data")
	source["metadata"].(object)["namespace"], source["metadata"].(object)["uid"] = "longhorn-canary-v1-1234", "source-claim"
	target["metadata"].(object)["namespace"], target["metadata"].(object)["uid"] = "longhorn-canary-v2-1234", "target-claim"
	op := resource("transfer")
	op["metadata"].(object)["namespace"] = "velero"
	op["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "velero.io/v1", "kind": kind, "name": text(parent, "metadata", "name"), "uid": text(parent, "metadata", "uid"), "controller": true}}
	op["status"] = object{"phase": "Completed", "completionTimestamp": "2026-10-29T22:00:00Z", "snapshotID": "repository-snapshot", "progress": object{"bytesDone": float64(134217800), "totalBytes": float64(134217800)}}
	op["spec"] = object{"backupStorageLocation": "default", "datamover": "velero", "sourceNamespace": text(source, "metadata", "namespace")}
	spec := op["spec"].(object)
	if kind == "Backup" {
		op["kind"] = "DataUpload"
		op["metadata"].(object)["labels"] = object{"velero.io/backup-uid": text(parent, "metadata", "uid"), "velero.io/pvc-uid": text(source, "metadata", "uid")}
		spec["snapshotType"], spec["sourcePVC"] = "CSI", "fio-data"
		spec["csiSnapshot"] = object{"driver": "driver.longhorn.io", "storageClass": "lh-canary-v1-1234", "snapshotClass": "lh-csi-canary-1234", "volumeSnapshot": "native-snapshot"}
	} else {
		op["kind"] = "DataDownload"
		op["metadata"].(object)["labels"] = object{"velero.io/restore-uid": text(parent, "metadata", "uid")}
		spec["targetVolume"] = object{"pvc": "fio-data", "namespace": text(target, "metadata", "namespace")}
		spec["snapshotID"], spec["snapshotSize"] = "repository-snapshot", float64(134217800)
		delete(op["status"].(object), "snapshotID")
	}
	return parent, source, target, op
}

func TestCSICanaryBindsBothNativeDataMovementOperations(t *testing.T) {
	for _, kind := range []string{"Backup", "Restore"} {
		parent, source, target, op := csiTransferFixture(kind)
		found, snapshot, err := verifyCSITransfer([]object{op}, parent, source, target, "repository-snapshot", "lh-canary-v1-1234", "lh-csi-canary-1234")
		if err != nil || found == nil || snapshot != "repository-snapshot" {
			t.Fatalf("valid %s transfer rejected: %v", kind, err)
		}
	}
}

func TestCSICanaryRejectsForeignOrIncompleteVolumeTransfers(t *testing.T) {
	for _, kind := range []string{"Backup", "Restore"} {
		for name, change := range map[string]func(object){
			"foreign owner":          func(o object) { value(o, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "another-backup" },
			"foreign repository":     func(o object) { o["spec"].(object)["backupStorageLocation"] = "another-destination" },
			"wrong source namespace": func(o object) { o["spec"].(object)["sourceNamespace"] = "production" },
			"incomplete payload":     func(o object) { value(o, "status", "progress").(object)["bytesDone"] = float64(128) },
			"uncompleted operation":  func(o object) { o["status"].(object)["phase"] = "InProgress" },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				parent, source, target, op := csiTransferFixture(kind)
				change(op)
				if _, _, err := verifyCSITransfer([]object{op}, parent, source, target, "repository-snapshot", "lh-canary-v1-1234", "lh-csi-canary-1234"); err == nil {
					t.Fatal("unproven CSI recovery accepted")
				}
			})
		}
	}
	for _, change := range []func(object){
		func(o object) { o["metadata"].(object)["labels"].(object)["velero.io/pvc-uid"] = "replaced-claim" },
		func(o object) { value(o, "spec", "csiSnapshot").(object)["driver"] = "another-driver" },
		func(o object) { value(o, "spec", "csiSnapshot").(object)["snapshotClass"] = "original-history" },
		func(o object) { o["status"].(object)["snapshotID"] = "" },
	} {
		parent, source, target, op := csiTransferFixture("Backup")
		change(op)
		if _, _, err := verifyCSITransfer([]object{op}, parent, source, target, "repository-snapshot", "lh-canary-v1-1234", "lh-csi-canary-1234"); err == nil {
			t.Fatal("foreign upload accepted")
		}
	}
	for _, change := range []func(object){
		func(o object) { value(o, "spec", "targetVolume").(object)["namespace"] = "production" },
		func(o object) { value(o, "spec", "targetVolume").(object)["pvc"] = "another-claim" },
		func(o object) { o["spec"].(object)["snapshotID"] = "another-point-in-time" },
	} {
		parent, source, target, op := csiTransferFixture("Restore")
		change(op)
		if _, _, err := verifyCSITransfer([]object{op}, parent, source, target, "repository-snapshot", "lh-canary-v1-1234", "lh-csi-canary-1234"); err == nil {
			t.Fatal("foreign download accepted")
		}
	}
}
