package main

import "encoding/json"

const nativeRestoreReadScript = nativeSeedVerify + `printf 'CANARY_RESTORED\n'
while :; do sleep 10; done
`

// The restore procedure owns and verifies the fresh V2 claim/backend and
// all three tagged targets before starting the copy.
// Restore only the Pod: its archived PVC remains available to the additional
// item lookup but is explicitly excluded from restoration. Velero must fill
// the new claim through the original Kopia snapshot rather than restore a PV.
func nativeRestoreAssets(record benchmarkRecord, seed canarySeedState, run string, targets map[string]string) ([]object, error) {
	if seed.validate(record) != nil || seed.Phase != "source-cleaned" || !benchmarkRunPattern.MatchString(run) || run == record.Run {
		return nil, errSeedEvidence
	}
	firstNode, err := nativeRestoreConsumerNode(targets, "")
	if err != nil {
		return nil, err
	}
	objects, err := benchmarkResources("v2", run, benchmarkNamespace)
	if err != nil {
		return nil, err
	}
	name := "longhorn-native-v2-" + run
	var assets []object
	for _, asset := range objects {
		switch text(asset, "kind") {
		case "NetworkPolicy":
			// The V1 source cleanup removes its own policy. Re-create and
			// own the deny policy before delegating the new Pod to Velero.
			assets = append(assets, asset)
		case "StorageClass":
			asset["metadata"].(object)["name"] = name
			asset["parameters"].(object)["nodeSelector"] = name
			// This functional canary pre-owns the independently provisioned
			// backend before delegating Pod creation to the restore controller.
			asset["volumeBindingMode"] = "Immediate"
			assets = append(assets, asset)
		case "PersistentVolumeClaim":
			asset["spec"].(object)["storageClassName"] = name
			assets = append(assets, asset)
		}
	}
	sourceAssets, err := benchmarkScratchAssets(client{nativeSeed: true}, "v1", record.Run, benchmarkNamespace, record.clientNode())
	if err != nil {
		return nil, err
	}
	var containers []any
	for _, asset := range sourceAssets {
		if text(asset, "kind") == "Pod" {
			containers = value(asset, "spec", "containers").([]any)
		}
	}
	if len(containers) != 1 {
		return nil, errSeedEvidence
	}
	containers[0].(object)["args"] = []any{nativeRestoreReadScript}
	containers[0].(object)["env"] = nativeRestorePodUIDEnvironment()
	patch, err := json.Marshal(object{
		"metadata": object{"labels": object{"storage.platform.devantler.tech/benchmark-run": run}},
		"spec":     object{"containers": containers, "nodeSelector": object{"kubernetes.io/hostname": firstNode, migrationPoolLabel: nil}},
	})
	if err != nil {
		return nil, err
	}
	modifiers, err := json.Marshal(object{"version": "v1", "resourceModifierRules": []any{object{
		"conditions": object{"groupResource": "pods", "resourceNameRegex": "^fio$", "namespaces": []any{benchmarkNamespace},
			"labelSelector": object{"matchLabels": object{"storage.platform.devantler.tech/benchmark-run": record.Run}}},
		"mergePatches": []any{object{"patchData": string(patch)}},
	}}})
	if err != nil {
		return nil, err
	}
	meta := func(suffix string) object {
		return object{"name": name + suffix, "namespace": "velero", "labels": object{
			"storage.platform.devantler.tech/benchmark-run": run, "velero.io/exclude-from-backup": "true"}}
	}
	assets = append(assets,
		object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("-modifiers"), "data": object{"modifiers.json": string(modifiers)}},
		object{"apiVersion": "velero.io/v1", "kind": "Restore", "metadata": meta(""), "spec": object{
			"backupName": seed.Backup.Name, "includedNamespaces": []any{benchmarkNamespace}, "includedResources": []any{"pods"},
			"excludedResources": []any{"persistentvolumeclaims", "persistentvolumes"}, "includeClusterResources": false, "restorePVs": false,
			"existingResourcePolicy": "none", "itemOperationTimeout": "20m0s",
			"labelSelector":    object{"matchLabels": object{"storage.platform.devantler.tech/benchmark-run": record.Run}},
			"resourceModifier": object{"kind": "ConfigMap", "name": name + "-modifiers"},
		}},
	)
	return assets, nil
}
