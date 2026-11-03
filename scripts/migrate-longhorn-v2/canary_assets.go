package main

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const canaryScript = `set -eu
while [ ! -f /tmp/canary-start ]; do sleep 2; done
fio --name=longhorn-v2-canary --filename=/data/payload.bin --size=128M --rw=write --bs=4k --ioengine=libaio --direct=1 --verify=sha256 --do_verify=1 --verify_fatal=1 --verify_state_save=0 --end_fsync=1 --output=/tmp/probe.json --output-format=json
cd /data
sha256sum payload.bin > payload.sha256
printf 'longhorn-v2-5582\n' > sentinel
sync
printf 'CANARY_READY\n'
while :; do sleep 10; done
`

const canaryVerify = `set -eu
cd /data
test "$(cat sentinel)" = longhorn-v2-5582
test "$(wc -c < payload.bin)" -eq 134217728
sha256sum --check payload.sha256
`

// canaryProbeResources reuses the reviewed restricted benchmark workload while
// giving this recovery proof its own names, script, and backup-visible labels.
func canaryProbeResources(namespace, class, runID, phase string) ([]object, error) {
	assets, err := benchmarkResources(phase, runID, "longhorn-benchmark-"+phase+"-"+runID)
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		metadata := asset["metadata"].(object)
		delete(metadata["labels"].(object), "velero.io/exclude-from-backup")
		metadata["labels"].(object)["storage.platform.devantler.tech/canary-run"] = runID
		if text(asset, "kind") == "Namespace" {
			metadata["name"] = namespace
		} else if text(asset, "kind") == "StorageClass" {
			metadata["name"] = class
			asset["parameters"].(object)["nodeSelector"] = ""
			if phase == "v2" {
				asset["parameters"].(object)["unmapMarkSnapChainRemoved"] = "disabled"
			}
		} else {
			metadata["namespace"] = namespace
		}
	}
	assets[3]["spec"].(object)["storageClassName"] = class
	value(assets[4], "spec", "containers").([]any)[0].(object)["args"] = []any{canaryScript}
	return assets, nil
}

// canaryResources limits backup and restore to this run's disposable workload.
func canaryResources(runID string) ([]object, error) {
	if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(runID) {
		return nil, errors.New("immutable canary run identity is required")
	}
	sourceNS, targetNS := "longhorn-canary-v1-"+runID, "longhorn-canary-v2-"+runID
	sourceClass, targetClass := "lh-canary-v1-"+runID, "lh-canary-v2-"+runID
	assets, err := canaryProbeResources(sourceNS, sourceClass, runID, "v1")
	if err != nil {
		return nil, err
	}
	pod := assets[4]
	pod["metadata"].(object)["annotations"] = object{"backup.velero.io/backup-volumes": "data"}
	pod["spec"].(object)["activeDeadlineSeconds"] = 5400
	target, err := canaryProbeResources(targetNS, targetClass, runID, "v2")
	if err != nil {
		return nil, err
	}
	assets = append(assets, target[:3]...)
	name := "longhorn-v2-canary-" + runID
	meta := func() object {
		return object{"name": name, "namespace": "velero", "labels": object{"storage.platform.devantler.tech/canary-run": runID}}
	}
	rules := fmt.Sprintf(`version: v1
resourceModifierRules:
- conditions:
    groupResource: persistentvolumeclaims
    resourceNameRegex: "^fio-data$"
    namespaces: ["%s"]
  patches:
  - operation: test
    path: /spec/storageClassName
    value: "%s"
  - operation: replace
    path: /spec/storageClassName
    value: "%s"
- conditions:
    groupResource: pods
    resourceNameRegex: "^fio$"
    namespaces: ["%s"]
  patches:
  - operation: test
    path: /spec/containers/0/image
    value: "%s"
  - operation: replace
    path: /spec/nodeSelector
    value: '{"kubernetes.io/hostname":"prod-worker-3"}'
`, sourceNS, sourceClass, targetClass, sourceNS, fioImage)
	assets = append(assets,
		object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta(), "data": object{"rules.yaml": rules}},
		object{"apiVersion": "velero.io/v1", "kind": "Backup", "metadata": meta(), "spec": object{
			"includedNamespaces": []any{sourceNS}, "includedResources": []any{"pods", "persistentvolumeclaims"}, "includeClusterResources": false,
			"storageLocation": "default", "snapshotVolumes": false, "defaultVolumesToFsBackup": false, "ttl": "24h0m0s", "csiSnapshotTimeout": "10m0s", "itemOperationTimeout": "30m0s"}},
		object{"apiVersion": "velero.io/v1", "kind": "Restore", "metadata": meta(), "spec": object{
			"backupName": name, "includedNamespaces": []any{sourceNS}, "includedResources": []any{"pods", "persistentvolumeclaims"}, "includeClusterResources": false,
			"namespaceMapping": object{sourceNS: targetNS}, "resourceModifier": object{"kind": "ConfigMap", "name": name}, "itemOperationTimeout": "30m0s"}},
	)
	return assets, nil
}

// cleanVeleroResult requires native completion without errors or warnings;
// Velero's omitted zero counters remain valid.
func cleanVeleroResult(o object) error {
	if text(o, "metadata", "uid") == "" || value(o, "metadata", "deletionTimestamp") != nil || text(o, "status", "phase") != "Completed" {
		return errors.New("native backup or restore has not completed")
	}
	if _, err := time.Parse(time.RFC3339, text(o, "status", "completionTimestamp")); err != nil {
		return errors.New("native backup or restore completion is missing")
	}
	// Velero v1.18 serializes its zero error/warning counters with omitempty.
	for _, field := range []string{"errors", "warnings"} {
		if count := value(o, "status", field); count != nil && count != float64(0) {
			return errors.New("native backup or restore reported warnings or errors")
		}
	}
	return nil
}
