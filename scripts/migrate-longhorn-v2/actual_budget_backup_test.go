package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestActualBudgetBackupUsesOnlyTheReviewedSnapshotPath(t *testing.T) {
	raw, err := os.ReadFile("../../k8s/providers/hetzner/infrastructure/controllers/velero/config-map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cm object
	if err = yaml.UnmarshalStrict(raw, &cm); err != nil {
		t.Fatal(err)
	}
	meta := valueObject(cm, "metadata")
	meta["uid"], meta["resourceVersion"] = "070c2277-cc94-4c67-b54b-14ac7d742d10", "1234"
	if err := validateActualBudgetBackupPolicy(cm); err != nil {
		t.Fatalf("reviewed snapshot policy did not qualify: %v", err)
	}
	valueObject(cm, "data")["policy.yaml"] = strings.ReplaceAll(text(cm, "data", "policy.yaml"), "type: snapshot", "type: fs-backup")
	if err := validateActualBudgetBackupPolicy(cm); err == nil {
		t.Fatal("copying live SQLite files qualified the protected backup")
	}
	spec := valueObject(actualBudgetBackup("12345"), "spec")
	if spec["snapshotVolumes"] != true || spec["snapshotMoveData"] != true || spec["defaultVolumesToFsBackup"] != false {
		t.Fatal("the application proof must take a CSI snapshot and transfer it off site")
	}
}

func TestActualBudgetBackupRequiresExactOffsiteTransfer(t *testing.T) {
	backup := actualBudgetBackup("12345")
	meta := valueObject(backup, "metadata")
	meta["uid"], meta["resourceVersion"] = "bcc16393-bb96-4208-95d1-1422ac960811", "128"
	name := text(backup, "metadata", "name")
	const claimUID = "b6e32d23-0001-47cc-9848-140e4ad09580"
	for _, fault := range []string{"", "different claim", "different namespace", "different backup", "other target", "short transfer", "empty snapshot", "alternate driver", "alternate class", "no CSI"} {
		t.Run(fault, func(t *testing.T) {
			owner := object{"apiVersion": "velero.io/v1", "kind": "Backup", "name": name, "uid": meta["uid"], "controller": true}
			snapshot := object{"driver": "driver.longhorn.io", "snapshotClass": "longhorn-snapshot-vsc",
				"storageClass": "longhorn", "volumeSnapshot": "actual-budget-snapshot"}
			payload := object{"sourceNamespace": actualBudgetNamespace, "sourcePVC": actualBudgetClaim,
				"snapshotType": "CSI", "csiSnapshot": snapshot, "backupStorageLocation": "default"}
			labels := object{"velero.io/pvc-uid": claimUID, "velero.io/backup-uid": meta["uid"], "velero.io/backup-name": name}
			progress := object{"bytesDone": float64(123), "totalBytes": float64(123)}
			status := object{"progress": progress, "phase": "Completed", "snapshotID": "snapshot",
				"completionTimestamp": "2026-11-26T15:00:04Z"}
			transfer := object{"apiVersion": "velero.io/v2alpha1", "kind": "DataUpload", "metadata": object{
				"namespace": "velero", "name": "budget-transfer", "uid": "transfer-uid", "resourceVersion": "50",
				"labels": labels, "ownerReferences": []any{owner}}, "spec": payload, "status": status}
			switch fault {
			case "different claim":
				labels["velero.io/pvc-uid"] = "another"
			case "different namespace":
				payload["sourceNamespace"] = "another"
			case "different backup":
				owner["uid"] = "another"
			case "other target":
				payload["backupStorageLocation"] = "another"
			case "short transfer":
				progress["bytesDone"] = float64(122)
			case "empty snapshot":
				status["snapshotID"] = ""
			case "alternate driver":
				snapshot["driver"] = "other"
			case "alternate class":
				snapshot["snapshotClass"] = "other"
			case "no CSI":
				payload["snapshotType"] = "other"
			}
			if got := actualBudgetTransfer(transfer, backup, claimUID); got != (fault == "") {
				t.Fatalf("unexpected transfer verdict %t", got)
			}
		})
	}
}

func TestActualBudgetBackupRequiresTenGiBOfLogicalAndPhysicalCapacityWithAMargin(t *testing.T) {
	const gib = int64(1 << 30)
	const worker = "test-worker"
	replica := object{"metadata": object{"name": "existing-replica"}, "spec": object{
		"nodeID": worker, "diskID": "native-disk", "dataEngine": "v1", "volumeSize": "129922760704",
	}}
	for _, tt := range []struct {
		name      string
		available int64
		reserved  int64
		ledger    object
		wantFit   bool
	}{
		{"clone fits", 53 * gib, 17 * gib, object{"existing-replica": float64(121 * gib)}, true},
		{"only eight GiB can schedule", 53 * gib, 19 * gib, object{"existing-replica": float64(121 * gib)}, false},
		{"unallocated bytes do not protect the physical margin", 50 * gib, 17 * gib, object{"existing-replica": float64(121 * gib)}, false},
		{"unattributed allocation", 53 * gib, 17 * gib, object{"replacement": float64(121 * gib)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			disk := object{"diskUUID": "native-disk", "storageMaximum": float64(148 * gib), "storageAvailable": float64(tt.available),
				"storageScheduled": float64(121 * gib), "scheduledReplica": tt.ledger}
			if fit := fitsActualBudgetSnapshot(disk, tt.reserved, []object{replica}, worker); fit != tt.wantFit {
				t.Fatalf("capacity accepted=%t, want %t", fit, tt.wantFit)
			}
		})
	}
}
