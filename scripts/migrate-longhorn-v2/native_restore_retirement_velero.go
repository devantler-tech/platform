package main

import "reflect"

func verifyNativeRestoreRetirementVelero(record benchmarkRecord, seed canarySeedState, state nativeRestoreState,
	restore, modifier object, transfers, repositories []object) error {
	if state.Phase != "completed" || state.validate(record, seed) != nil ||
		benchmarkIdentity(modifier) != state.Resources["ConfigMap"] || text(modifier, "apiVersion") != "v1" ||
		text(modifier, "kind") != "ConfigMap" || !seedVeleroIdentity(modifier) {
		return errNativeRestoreEvidence
	}
	assets, err := nativeRestoreRecordedAssets(record, seed, state)
	if err != nil || verifyNativeRestoreResult(state, restore, assets) != nil {
		return errNativeRestoreEvidence
	}
	for _, asset := range assets {
		if text(asset, "kind") == "ConfigMap" && (!reflect.DeepEqual(asset["data"], modifier["data"]) ||
			!reflect.DeepEqual(value(asset, "metadata", "labels"), value(modifier, "metadata", "labels"))) {
			return errNativeRestoreEvidence
		}
	}
	// Use the recorded Velero consumer, which may have expired before the
	// replacement performed the first checksum and reattachment.
	first := state.Resources["FirstPod"]
	if state.Expired != nil {
		first = state.Expired.Pod
	}
	podReference := object{"metadata": object{"name": first.Name, "namespace": first.Namespace, "uid": first.UID}}
	transfer, err := originalNativeRestoreTransfer(record, seed, state, restore, podReference, transfers, repositories)
	owners, _ := value(transfer, "metadata", "ownerReferences").([]any)
	if err != nil || len(owners) != 1 {
		return errNativeRestoreEvidence
	}
	return nil
}
