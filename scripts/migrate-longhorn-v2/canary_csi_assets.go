package main

// csiCanaryResources selects native snapshot data movement for the same
// disposable byte-recovery probe and gives it a run-owned snapshot class.
func csiCanaryResources(run string) ([]object, error) {
	assets, err := canaryResources(run)
	if err != nil {
		return nil, err
	}
	delete(assets[4]["metadata"].(object), "annotations")
	class := "lh-csi-canary-" + run
	assets[9]["metadata"].(object)["annotations"] = object{"velero.io/csi-volumesnapshot-class_driver.longhorn.io": class}
	spec := assets[9]["spec"].(object)
	spec["snapshotVolumes"], spec["snapshotMoveData"], spec["dataMover"] = true, true, "velero"
	assets[10]["spec"].(object)["restorePVs"] = true
	return append(assets, object{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotClass",
		"metadata": object{"name": class, "labels": object{"storage.platform.devantler.tech/canary-run": run}},
		"driver":   "driver.longhorn.io", "deletionPolicy": "Delete", "parameters": object{"type": "snap"},
	}), nil
}
