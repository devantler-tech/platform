package main

// The caller verifies the original completed Backup and DataUpload together.
// A successful Restore also needs one completed copy of that exact snapshot
// into the isolated claim. Velero counts completion even when no data mover
// ran, such as when a target claim already existed.
func verifyActualBudgetRestoreDownload(restore object, transfers []object, namespace, backup, snapshot string) error {
	id := benchmarkIdentity(restore)
	if snapshot == "" || namespace == actualBudgetNamespace || id.Name != namespace ||
		!seedVeleroIdentity(restore) || !nativeRestoreUID(id.UID) ||
		text(restore, "apiVersion") != "velero.io/v1" || text(restore, "kind") != "Restore" ||
		text(restore, "spec", "backupName") != backup || cleanVeleroResult(restore) != nil {
		return errActualBudgetBackup
	}
	matched := 0
	for _, transfer := range transfers {
		labels := valueObject(valueObject(transfer, "metadata"), "labels")
		owned := controlledBy(transfer, restore)
		if !owned && text(labels, "velero.io/restore-name") != id.Name && text(labels, "velero.io/restore-uid") != id.UID {
			continue
		}
		spec := valueObject(transfer, "spec")
		done, validDone := nativeObservationUint(value(transfer, "status", "progress", "bytesDone"))
		total, validTotal := nativeObservationUint(value(transfer, "status", "progress", "totalBytes"))
		mover := text(spec, "datamover")
		if !owned || !seedVeleroIdentity(transfer) || !nativeRestoreUID(text(transfer, "metadata", "uid")) ||
			text(transfer, "apiVersion") != "velero.io/v2alpha1" || text(transfer, "kind") != "DataDownload" ||
			text(labels, "velero.io/restore-name") != id.Name || text(labels, "velero.io/restore-uid") != id.UID ||
			text(spec, "backupStorageLocation") != "default" || text(spec, "sourceNamespace") != actualBudgetNamespace ||
			text(spec, "targetVolume", "namespace") != namespace || text(spec, "targetVolume", "pvc") != actualBudgetClaim ||
			text(spec, "snapshotID") != snapshot || value(spec, "cancel") == true || (mover != "" && mover != "velero") ||
			!validDone || !validTotal || done == 0 || total != done || cleanVeleroResult(transfer) != nil {
			return errActualBudgetBackup
		}
		matched++
	}
	if matched != 1 {
		return errActualBudgetBackup
	}
	return nil
}
