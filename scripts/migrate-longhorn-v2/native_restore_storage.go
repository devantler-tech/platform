package main

import "reflect"

// Bind the independently provisioned target to the restore journal's original
// creation receipts. This checks storage provenance, not replica health, live
// target identities, physical source absence or completed Kopia restoration.
func verifyNativeRestoreStorage(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, claim, class, pv, volume object) error {
	if state.validate(record, seed) != nil || nativeRestorePhaseIndex(state.Phase) < nativeRestorePhaseIndex("storage-bound") {
		return errNativeRestoreEvidence
	}
	for kind, actual := range map[string]object{"PersistentVolumeClaim": claim, "StorageClass": class, "PersistentVolume": pv, "LonghornVolume": volume} {
		api, nativeKind := "v1", kind
		switch kind {
		case "StorageClass":
			api = "storage.k8s.io/v1"
		case "LonghornVolume":
			api, nativeKind = "longhorn.io/v1beta2", "Volume"
		}
		if benchmarkIdentity(actual) != state.Resources[kind] || text(actual, "apiVersion") != api || text(actual, "kind") != nativeKind ||
			text(actual, "metadata", "resourceVersion") == "" || value(actual, "metadata", "deletionTimestamp") != nil {
			return errNativeRestoreEvidence
		}
	}
	targets := map[string]string{}
	for _, target := range state.Targets {
		targets[target.Name] = target.UID
	}
	assets, err := nativeRestoreAssets(record, seed, state.Run, targets)
	if err != nil {
		return err
	}
	for _, desired := range assets {
		if text(desired, "kind") != "StorageClass" {
			continue
		}
		for _, key := range []string{"provisioner", "parameters", "reclaimPolicy", "volumeBindingMode", "allowVolumeExpansion", "mountOptions", "allowedTopologies"} {
			if !reflect.DeepEqual(value(class, key), value(desired, key)) {
				return errNativeRestoreEvidence
			}
		}
	}
	if text(claim, "status", "phase") != "Bound" || text(claim, "spec", "storageClassName") != state.Resources["StorageClass"].Name ||
		text(claim, "spec", "volumeName") != state.Resources["PersistentVolume"].Name ||
		text(claim, "spec", "resources", "requests", "storage") != "4Gi" || value(claim, "spec", "dataSource") != nil || value(claim, "spec", "dataSourceRef") != nil ||
		text(claim, "spec", "volumeMode") != "Filesystem" || !reflect.DeepEqual(value(claim, "spec", "accessModes"), []any{"ReadWriteOnce"}) ||
		text(pv, "spec", "capacity", "storage") != "4Gi" || text(pv, "spec", "volumeMode") != "Filesystem" ||
		!reflect.DeepEqual(value(pv, "spec", "accessModes"), []any{"ReadWriteOnce"}) || text(pv, "spec", "csi", "fsType") != "ext4" ||
		text(volume, "spec", "dataEngine") != "v2" || text(volume, "spec", "accessMode") != "rwo" || text(volume, "spec", "size") != "4294967296" ||
		value(volume, "spec", "encrypted") != false || value(volume, "spec", "numberOfReplicas") != float64(3) ||
		!reflect.DeepEqual(value(volume, "spec", "nodeSelector"), []any{state.Resources["StorageClass"].Name}) ||
		value(volume, "spec", "fromBackup") != "" || value(volume, "spec", "dataSource") != "" || value(volume, "spec", "backingImage") != "" ||
		value(volume, "spec", "cloneMode") != nil && value(volume, "spec", "cloneMode") != "" || value(volume, "status", "restoreRequired") == true ||
		text(volume, "status", "kubernetesStatus", "namespace") != benchmarkNamespace || text(volume, "status", "kubernetesStatus", "pvcName") != "fio-data" ||
		verifyScratchBinding(pv, volume, claim, class) != nil {
		return errNativeRestoreEvidence
	}
	return nil
}
