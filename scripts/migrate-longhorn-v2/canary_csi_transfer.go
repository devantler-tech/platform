package main

import "errors"

// CSI DataUpload/DataDownload have different schemas from PVB/PVR. Bind the
// actual native fields instead of inferring a CSI recovery from FSB evidence.
func verifyCSITransfer(transfers []object, parent, sourceClaim, targetClaim object, expectedSnapshot, sourceClass, snapshotClass string) (object, string, error) {
	kind, uid := text(parent, "kind"), text(parent, "metadata", "uid")
	if uid == "" || text(parent, "metadata", "namespace") != "velero" || text(sourceClaim, "metadata", "uid") == "" || (kind != "Backup" && kind != "Restore") {
		return nil, "", errors.New("CSI transfer parent or source claim identity is missing")
	}
	label, expectedKind := "velero.io/backup-uid", "DataUpload"
	if kind == "Restore" {
		label, expectedKind = "velero.io/restore-uid", "DataDownload"
		if text(targetClaim, "metadata", "uid") == "" || text(targetClaim, "metadata", "uid") == text(sourceClaim, "metadata", "uid") {
			return nil, "", errors.New("CSI restore requires an independent target claim")
		}
	}
	var found object
	snapshot := ""
	for _, op := range transfers {
		if !controlledBy(op, parent) && text(op, "metadata", "labels", label) != uid {
			continue
		}
		if found != nil || !controlledBy(op, parent) || text(op, "metadata", "namespace") != "velero" || text(op, "kind") != expectedKind || text(op, "metadata", "labels", label) != uid ||
			text(op, "metadata", "uid") == "" || text(op, "spec", "backupStorageLocation") != "default" || text(op, "spec", "datamover") != "velero" || text(op, "spec", "sourceNamespace") != text(sourceClaim, "metadata", "namespace") {
			return nil, "", errors.New("CSI transfer ownership, repository or source scope is ambiguous")
		}
		if err := cleanVeleroResult(op); err != nil {
			return nil, "", err
		}
		done, doneOK := value(op, "status", "progress", "bytesDone").(float64)
		total, totalOK := value(op, "status", "progress", "totalBytes").(float64)
		if !doneOK || !totalOK || done < 134217728 || done != total {
			return nil, "", errors.New("the CSI operation has not transferred the complete canary payload")
		}
		if kind == "Backup" {
			if text(op, "metadata", "labels", "velero.io/pvc-uid") != text(sourceClaim, "metadata", "uid") || text(op, "spec", "snapshotType") != "CSI" ||
				text(op, "spec", "sourcePVC") != text(sourceClaim, "metadata", "name") || text(op, "spec", "csiSnapshot", "driver") != "driver.longhorn.io" ||
				text(op, "spec", "csiSnapshot", "storageClass") != sourceClass || text(op, "spec", "csiSnapshot", "snapshotClass") != snapshotClass ||
				!kubeName.MatchString(text(op, "spec", "csiSnapshot", "volumeSnapshot")) {
				return nil, "", errors.New("CSI upload does not bind the exact source claim and disposable snapshot class")
			}
			snapshot = text(op, "status", "snapshotID")
		} else {
			if text(op, "spec", "targetVolume", "namespace") != text(targetClaim, "metadata", "namespace") || text(op, "spec", "targetVolume", "pvc") != text(targetClaim, "metadata", "name") ||
				text(op, "spec", "snapshotID") != expectedSnapshot || expectedSnapshot == "" {
				return nil, "", errors.New("CSI download does not bind the exact repository snapshot and target claim")
			}
			size, ok := value(op, "spec", "snapshotSize").(float64)
			if !ok || size < 134217728 {
				return nil, "", errors.New("CSI download source size is incomplete")
			}
			snapshot = text(op, "spec", "snapshotID")
		}
		if snapshot == "" {
			return nil, "", errors.New("CSI repository snapshot identity is missing")
		}
		found = op
	}
	if found == nil {
		return nil, "", errors.New("exactly one completed CSI volume-data operation is required")
	}
	return found, snapshot, nil
}
