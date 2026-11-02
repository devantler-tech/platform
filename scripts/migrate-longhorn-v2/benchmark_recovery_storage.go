package main

import (
	"errors"
	"reflect"
)

// This checks provenance only. Live attachment/consumer inventories, recovery
// ownership, current source and deletion intents still precede any retirement.
func verifyRecoveredBenchmarkStorage(record benchmarkRecord, claim, class, pv, volume object) error {
	if record.PendingCreate != "" {
		return errors.New("benchmark has an unresolved CREATE; preserve its assets")
	}
	for kind, actual := range map[string]object{"PersistentVolumeClaim": claim, "StorageClass": class, "PersistentVolume": pv, "LonghornVolume": volume} {
		identity, known := record.Resources[kind]
		if !known || identity.UID == "" || benchmarkIdentity(actual) != identity {
			return errors.New("recovery storage differs from its original UID receipts")
		}
	}
	if text(claim, "metadata", "namespace") != record.Namespace || text(claim, "metadata", "name") != "fio-data" || text(claim, "spec", "volumeName") != text(pv, "metadata", "name") || text(volume, "metadata", "namespace") != "longhorn-system" || text(volume, "spec", "dataEngine") != record.Phase {
		return errors.New("recovery storage scope or engine differs")
	}
	assets, err := benchmarkResources(record.Phase, record.Run, record.Namespace)
	if err != nil {
		return err
	}
	for _, desired := range assets {
		if text(desired, "kind") != "StorageClass" {
			continue
		}
		if text(class, "metadata", "name") != text(desired, "metadata", "name") {
			return errors.New("recovery storage class differs")
		}
		for _, key := range []string{"provisioner", "parameters", "reclaimPolicy", "volumeBindingMode", "allowVolumeExpansion"} {
			if !reflect.DeepEqual(value(class, key), value(desired, key)) {
				return errors.New("recovery storage class retention or provisioning changed")
			}
		}
	}
	return verifyScratchBinding(pv, volume, claim, class)
}
