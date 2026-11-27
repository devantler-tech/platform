package main

import "testing"

func TestActualBudgetRestoreBindsTheNewDownloadToTheOriginalSnapshot(t *testing.T) {
	for _, fault := range []string{"", "owner", "snapshot", "namespace", "PVC", "wrong repository", "short transfer", "second transfer", "another backup"} {
		t.Run(fault, func(t *testing.T) {
			const namespace = "actual-budget-recovery-91022"
			const restoreUID = "b1908b4f-f917-4016-936e-e801f14ef8a0"
			restore := object{"apiVersion": "velero.io/v1", "kind": "Restore",
				"metadata": object{"uid": restoreUID, "name": namespace, "namespace": "velero", "resourceVersion": "2"},
				"spec":     object{"backupName": "actual-budget-v1-90718"},
				"status":   object{"phase": "Completed", "completionTimestamp": "2026-11-27T06:00:07Z"}}
			owner := object{"uid": restoreUID, "name": namespace, "apiVersion": "velero.io/v1", "kind": "Restore", "controller": true}
			target := object{"pvc": actualBudgetClaim, "namespace": namespace}
			spec := object{"targetVolume": target, "backupStorageLocation": "default", "sourceNamespace": actualBudgetNamespace, "snapshotID": "snapshot"}
			progress := object{"totalBytes": float64(42), "bytesDone": float64(42)}
			transfer := object{"apiVersion": "velero.io/v2alpha1", "kind": "DataDownload",
				"metadata": object{"uid": "f374b76f-884d-4b33-b12d-ba759cbca96b", "name": "restore-copy", "namespace": "velero", "resourceVersion": "9",
					"labels": object{"velero.io/restore-name": namespace, "velero.io/restore-uid": restoreUID}, "ownerReferences": []any{owner}},
				"spec": spec, "status": object{"phase": "Completed", "progress": progress, "completionTimestamp": "2026-11-27T06:00:03Z"}}
			transfers := []object{transfer}
			switch fault {
			case "owner":
				owner["uid"] = "another"
			case "snapshot":
				spec["snapshotID"] = "different"
			case "namespace":
				target["namespace"] = actualBudgetNamespace
			case "PVC":
				target["pvc"] = "rebound"
			case "wrong repository":
				spec["backupStorageLocation"] = "another"
			case "short transfer":
				progress["bytesDone"] = float64(41)
			case "second transfer":
				transfers = append(transfers, transfer)
			case "another backup":
				valueObject(restore, "spec")["backupName"] = "actual-budget-v1-90719"
			}
			if valid := verifyActualBudgetRestoreDownload(restore, transfers, namespace, "actual-budget-v1-90718", "snapshot") == nil; valid != (fault == "") {
				t.Fatalf("unexpected restore verdict %t", valid)
			}
		})
	}
}
