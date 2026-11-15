package main

import "reflect"

// retainedBackupSpec accepts the reviewed backup spec plus the defaults Velero fills in, and nothing else.
func retainedBackupSpec(want, actual object) bool {
	defaults := object{"csiSnapshotTimeout": "10m0s", "itemOperationTimeout": "4h0m0s", "snapshotMoveData": false, "volumeGroupSnapshotLabelKey": "velero.io/volume-group", "excludedResources": []any{"volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io"}}
	for key, value := range want {
		if !reflect.DeepEqual(actual[key], value) {
			return false
		}
	}
	for key, value := range actual {
		if _, present := want[key]; !present {
			if expected, known := defaults[key]; !known || !reflect.DeepEqual(value, expected) {
				return false
			}
		}
	}
	return true
}
