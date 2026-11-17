package main

import "reflect"

func verifyNativeSeedStorage(record benchmarkRecord, claim, class, pv, volume object) error {
	if record.Workload != nativeSeedWorkload() || record.Phase != "v1" || record.Version != 3 || record.Namespace != benchmarkNamespace ||
		verifyRecoveredBenchmarkStorage(record, claim, class, pv, volume) != nil ||
		text(claim, "spec", "resources", "requests", "storage") != "4Gi" || value(claim, "spec", "dataSource") != nil || value(claim, "spec", "dataSourceRef") != nil ||
		text(pv, "spec", "capacity", "storage") != "4Gi" || text(volume, "spec", "size") != "4294967296" || value(volume, "spec", "encrypted") != false ||
		value(volume, "spec", "fromBackup") != "" || value(volume, "spec", "dataSource") != "" || value(volume, "spec", "backingImage") != "" ||
		value(volume, "spec", "cloneMode") != nil && value(volume, "spec", "cloneMode") != "" || value(volume, "status", "restoreRequired") == true ||
		value(volume, "spec", "numberOfReplicas") != float64(3) || !reflect.DeepEqual(value(volume, "spec", "nodeSelector"), []any{benchmarkTag("v1", record.Run)}) ||
		text(volume, "status", "kubernetesStatus", "namespace") != benchmarkNamespace || text(volume, "status", "kubernetesStatus", "pvcName") != "fio-data" {
		return errSeedEvidence
	}
	return nil
}
