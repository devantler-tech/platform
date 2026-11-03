package main

import "testing"

func TestCSICanaryUsesItsOwnDisposableSnapshotAndNativeDataMover(t *testing.T) {
	assets, err := csiCanaryResources("1234")
	if err != nil || len(assets) != 12 {
		t.Fatalf("CSI fixture invalid: %v", err)
	}
	pod, backup, restore, class := assets[4], assets[9], assets[10], assets[11]
	if value(pod, "metadata", "annotations", "backup.velero.io/backup-volumes") != nil ||
		value(backup, "spec", "snapshotVolumes") != true || value(backup, "spec", "snapshotMoveData") != true || text(backup, "spec", "dataMover") != "velero" ||
		value(backup, "spec", "defaultVolumesToFsBackup") != false || value(restore, "spec", "restorePVs") != true {
		t.Fatal("CSI data movement was replaced by FSB or a metadata-only restore")
	}
	if text(class, "kind") != "VolumeSnapshotClass" || text(class, "metadata", "name") != "lh-csi-canary-1234" || text(class, "driver") != "driver.longhorn.io" || text(class, "deletionPolicy") != "Delete" || text(class, "parameters", "type") != "snap" ||
		text(backup, "metadata", "annotations", "velero.io/csi-volumesnapshot-class_driver.longhorn.io") != text(class, "metadata", "name") {
		t.Fatal("backup could select an original historical snapshot or a default snapshot class")
	}
	if value(class, "metadata", "labels", "velero.io/csi-volumesnapshot-class") != nil || value(class, "metadata", "annotations", "snapshot.storage.kubernetes.io/is-default-class") != nil {
		t.Fatal("disposable canary changed global snapshot-class selection")
	}
	for _, i := range []int{2, 7} {
		if text(assets[i], "reclaimPolicy") != "Retain" {
			t.Fatal("source or restored data class lost Retain")
		}
	}
	fsb, err := canaryResources("1234")
	if err != nil || value(fsb[9], "spec", "snapshotVolumes") != false || value(fsb[9], "spec", "snapshotMoveData") != nil {
		t.Fatal("FSB transport was changed by CSI mode")
	}
}
