package main

import "testing"

func TestRetainedBackupAcceptsObservedDefaultsAndRefusesExpandedScope(t *testing.T) {
	want := child(backupManifest(retainedRecovery), "spec")
	actual := object{}
	for k, v := range want {
		actual[k] = v
	}
	defaults := object{"csiSnapshotTimeout": "10m0s", "itemOperationTimeout": "4h0m0s", "snapshotMoveData": false, "volumeGroupSnapshotLabelKey": "velero.io/volume-group", "excludedResources": []any{"volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io"}}
	for k, v := range defaults {
		actual[k] = v
	}
	if !retainedBackupSpec(want, actual) {
		t.Fatal("native defaults refused")
	}
	actual["hooks"] = object{"resources": []any{object{"name": "foreign"}}}
	if retainedBackupSpec(want, actual) {
		t.Fatal("expanded backup scope accepted")
	}
}
