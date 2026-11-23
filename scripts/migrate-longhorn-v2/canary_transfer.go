package main

import (
	"errors"
	"math"
)

// controlledBy requires the exact native Velero parent as controller owner.
func controlledBy(o, parent object) bool {
	refs, _ := value(o, "metadata", "ownerReferences").([]any)
	for _, ref := range refs {
		r, _ := ref.(object)
		if text(r, "apiVersion") == "velero.io/v1" && text(r, "kind") == text(parent, "kind") && text(r, "name") == text(parent, "metadata", "name") &&
			text(r, "uid") == text(parent, "metadata", "uid") && text(r, "uid") != "" && value(r, "controller") == true {
			return true
		}
	}
	return false
}

// verifyCanaryTransfer binds one complete Kopia payload transfer to the original
// pod and backup destination; restores must name that exact source snapshot.
func verifyCanaryTransfer(transfers []object, parent, pod object, expectedSnapshot, sourceNS string) (string, error) {
	if text(parent, "apiVersion") != "velero.io/v1" || !seedVeleroIdentity(parent) || text(pod, "metadata", "uid") == "" || (text(parent, "kind") != "Backup" && text(parent, "kind") != "Restore") {
		return "", errors.New("canary transfer ownership is missing")
	}
	matched, snapshot := 0, ""
	for _, transfer := range transfers {
		owned := controlledBy(transfer, parent)
		if !owned && text(transfer, "spec", "pod", "uid") != text(pod, "metadata", "uid") {
			continue
		}
		matched++
		expectedKind := map[string]string{"Backup": "PodVolumeBackup", "Restore": "PodVolumeRestore"}[text(parent, "kind")]
		if !owned || text(transfer, "apiVersion") != "velero.io/v1" || text(transfer, "kind") != expectedKind || !seedVeleroIdentity(transfer) || text(transfer, "spec", "pod", "uid") != text(pod, "metadata", "uid") ||
			text(transfer, "spec", "pod", "namespace") != text(pod, "metadata", "namespace") || text(transfer, "spec", "pod", "name") != "fio" ||
			text(transfer, "spec", "volume") != "data" || text(transfer, "spec", "backupStorageLocation") != "default" || text(transfer, "spec", "repoIdentifier") != "" || text(transfer, "spec", "uploaderType") != "kopia" {
			return "", errors.New("canary data transfer belongs to another pod, volume, repository or operation")
		}
		if err := cleanVeleroResult(transfer); err != nil {
			return "", err
		}
		done, doneOK := value(transfer, "status", "progress", "bytesDone").(float64)
		total, totalOK := value(transfer, "status", "progress", "totalBytes").(float64)
		if !doneOK || !totalOK || math.IsNaN(done) || done < 134217728 || done > float64(v1ScratchBytes) || math.Trunc(done) != done || done != total {
			return "", errors.New("complete canary payload transfer is unproven")
		}
		snapshot = text(transfer, "status", "snapshotID")
		if text(parent, "kind") == "Restore" {
			snapshot = text(transfer, "spec", "snapshotID")
			if snapshot != expectedSnapshot || text(transfer, "spec", "sourceNamespace") != sourceNS {
				return "", errors.New("canary restore did not use the exact source snapshot")
			}
		}
		if snapshot == "" {
			return "", errors.New("canary volume-data snapshot identity is missing")
		}
	}
	if matched != 1 {
		return "", errors.New("exactly one completed canary volume-data transfer is required")
	}
	return snapshot, nil
}
