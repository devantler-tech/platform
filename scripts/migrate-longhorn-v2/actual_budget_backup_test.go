package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestActualBudgetBackupUsesOnlyTheReviewedFilesystemPath(t *testing.T) {
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
		t.Fatalf("reviewed filesystem policy did not qualify: %v", err)
	}
	valueObject(cm, "data")["policy.yaml"] = strings.ReplaceAll(text(cm, "data", "policy.yaml"), "fs-backup", "snapshot")
	if err := validateActualBudgetBackupPolicy(cm); err == nil {
		t.Fatal("the old snapshot policy still admitted the production backup")
	}
}

func TestActualBudgetBackupRequiresExactOffsiteTransfer(t *testing.T) {
	backup := actualBudgetBackup("12345")
	meta := valueObject(backup, "metadata")
	meta["uid"], meta["resourceVersion"] = "bcc16393-bb96-4208-95d1-1422ac960811", "128"
	name := text(backup, "metadata", "name")
	pod := object{"metadata": object{"name": "actual-budget-test-pod", "namespace": actualBudgetNamespace,
		"uid": "dc52af03-2391-417b-8e65-5d90521d403e"}}
	const claimUID = "b6e32d23-0001-47cc-9848-140e4ad09580"
	for _, fault := range []string{"", "different claim", "different pod", "different backup", "other target", "short transfer", "empty snapshot"} {
		t.Run(fault, func(t *testing.T) {
			owner := object{"apiVersion": "velero.io/v1", "kind": "Backup", "name": name, "uid": meta["uid"], "controller": true}
			payload := object{"pod": pod["metadata"], "volume": "data", "backupStorageLocation": "default", "uploaderType": "kopia"}
			labels := object{"velero.io/pvc-uid": claimUID}
			progress := object{"bytesDone": float64(123), "totalBytes": float64(123)}
			status := object{"progress": progress, "phase": "Completed", "snapshotID": "snapshot",
				"completionTimestamp": "2026-11-26T15:00:04Z"}
			transfer := object{"apiVersion": "velero.io/v1", "kind": "PodVolumeBackup", "metadata": object{
				"namespace": "velero", "name": "budget-transfer", "uid": "transfer-uid", "resourceVersion": "50",
				"labels": labels, "ownerReferences": []any{owner}}, "spec": payload, "status": status}
			switch fault {
			case "different claim":
				labels["velero.io/pvc-uid"] = "another"
			case "different pod":
				payload["pod"] = object{"namespace": actualBudgetNamespace, "name": "replaced-pod", "uid": "other"}
			case "different backup":
				owner["uid"] = "another"
			case "other target":
				payload["backupStorageLocation"] = "another"
			case "short transfer":
				progress["bytesDone"] = float64(122)
			case "empty snapshot":
				status["snapshotID"] = ""
			}
			if got := actualBudgetTransfer(transfer, backup, pod, claimUID); got != (fault == "") {
				t.Fatalf("unexpected transfer verdict %t", got)
			}
		})
	}
}
