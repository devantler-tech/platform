package main

// Reclaim checks the completed restore's exclusive storage binding after its
// second consumer has left. Any writer still needs a live reference fence and
// a compare-and-swap on the exact observed PV.
func verifyNativeRestoreReclaim(record benchmarkRecord, seed canarySeedState, state nativeRestoreState,
	detached nativeRestoreDetachedObservation, pvs, claims []object) error {
	if state.Phase != "completed" || text(detached.PV, "status", "phase") != "Bound" ||
		verifyNativeRestoreQuiescence(record, seed, state, detached) != nil {
		return errNativeRestoreEvidence
	}
	pv := state.Resources["PersistentVolume"]
	backend := state.Resources["LonghornVolume"]
	claim := state.Resources["PersistentVolumeClaim"]
	class := state.Resources["StorageClass"].Name
	pvMatches := 0
	for _, current := range pvs {
		id := benchmarkIdentity(current)
		if id.Name == "" || id.UID == "" {
			return errNativeRestoreEvidence
		}
		if id == pv {
			if text(current, "metadata", "resourceVersion") != text(detached.PV, "metadata", "resourceVersion") {
				return errNativeRestoreEvidence
			}
			pvMatches++
		} else if id.Name == pv.Name || id.UID == pv.UID || text(current, "spec", "csi", "volumeHandle") == backend.Name ||
			text(current, "spec", "storageClassName") == class {
			return errNativeRestoreEvidence
		}
	}
	claimMatches := 0
	for _, current := range claims {
		id := benchmarkIdentity(current)
		if id.Name == "" || id.UID == "" || id.Namespace == "" {
			return errNativeRestoreEvidence
		}
		if id == claim {
			if text(current, "metadata", "resourceVersion") != text(detached.Claim, "metadata", "resourceVersion") {
				return errNativeRestoreEvidence
			}
			claimMatches++
		} else if id.Namespace == benchmarkNamespace || id.UID == claim.UID || text(current, "spec", "volumeName") == pv.Name ||
			text(current, "spec", "storageClassName") == class {
			return errNativeRestoreEvidence
		}
	}
	if pvMatches != 1 || claimMatches != 1 {
		return errNativeRestoreEvidence
	}
	return nil
}
