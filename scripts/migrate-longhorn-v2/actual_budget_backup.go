package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	actualBudgetBackupOperation = "backup-actual-budget-v1"
	actualBudgetNamespace       = "actual-budget"
	actualBudgetClaim           = "actual-budget-actualbudget-data"
	v1BackupPolicy              = "velero-volume-policies"
)

var errActualBudgetBackup = errors.New("the exact Actual Budget recovery copy is unverified")

func authorizeActualBudgetBackup(getenv func(string) string) error {
	confirm := getenv("LONGHORN_V2_CONFIRM")
	if getenv("LONGHORN_V2_OPERATION") != actualBudgetBackupOperation || getenv("LONGHORN_V2_NODE") != "" ||
		getenv("LONGHORN_V2_DATABASE") != "" || !validBenchmarkOwner(getenv("GITHUB_RUN_ID"), getenv("GITHUB_SHA")) ||
		!strings.HasPrefix(confirm, actualBudgetBackupOperation+":") ||
		!retentionDigest.MatchString(strings.TrimPrefix(confirm, actualBudgetBackupOperation+":")) {
		return errActualBudgetBackup
	}
	return authorizeWorkflow(getenv, confirm)
}

func actualBudgetBackup(run string) object {
	return object{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": object{"name": "actual-budget-v1-" + run, "namespace": "velero",
			"labels": object{"storage.platform.devantler.tech/benchmark-run": run}},
		"spec": object{
			"includedNamespaces": []any{actualBudgetNamespace}, "includedResources": []any{"pods", "persistentvolumeclaims"},
			"includeClusterResources": false, "storageLocation": "default", "snapshotVolumes": false, "snapshotMoveData": false,
			"defaultVolumesToFsBackup": false, "ttl": "168h0m0s", "itemOperationTimeout": "20m0s",
			"labelSelector":  object{"matchLabels": object{"app.kubernetes.io/name": "actualbudget", "app.kubernetes.io/instance": actualBudgetNamespace}},
			"resourcePolicy": object{"kind": "configmap", "name": v1BackupPolicy},
		},
	}
}

// The confirmed source still has a single live reader and the original backend.
func observeActualBudgetBackupSource(ctx context.Context, c client, digest string) (object, string, error) {
	pv, err := c.observeRetainedSource(ctx, retentionTarget{actualBudgetNamespace, actualBudgetClaim, digest})
	if err != nil || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		text(pv, "spec", "storageClassName") != "longhorn" || !providerRWOFilesystem(pv) {
		return nil, "", errActualBudgetBackup
	}
	pods, err := c.list(ctx, "pods", actualBudgetNamespace)
	if err != nil {
		return nil, "", errActualBudgetBackup
	}
	var selected object
	for _, pod := range pods {
		labelled := text(pod, "metadata", "labels", "app.kubernetes.io/name") == "actualbudget" &&
			text(pod, "metadata", "labels", "app.kubernetes.io/instance") == actualBudgetNamespace
		mounts, _ := value(pod, "spec", "volumes").([]any)
		claims := 0
		uses := slices.ContainsFunc(mounts, func(raw any) bool {
			volume, _ := raw.(object)
			return text(volume, "persistentVolumeClaim", "claimName") == actualBudgetClaim
		})
		if !labelled && !uses {
			continue
		}
		for _, raw := range mounts {
			volume, _ := raw.(object)
			if value(volume, "persistentVolumeClaim") != nil {
				if text(volume, "name") != "data" || text(volume, "persistentVolumeClaim", "claimName") != actualBudgetClaim {
					return nil, "", errActualBudgetBackup
				}
				claims++
			}
		}
		if selected != nil || !labelled || !uses || claims != 1 ||
			!recoveredNativeObject(pod, "v1", "Pod", actualBudgetNamespace) ||
			text(pod, "status", "phase") != "Running" || !conditionsTrue(valueObject(pod, "status"), "Ready") ||
			value(pod, "spec", "hostUsers") != false {
			return nil, "", errActualBudgetBackup
		}
		selected = pod
	}
	if selected == nil {
		return nil, "", errActualBudgetBackup
	}
	return selected, text(pv, "spec", "claimRef", "uid"), nil
}

func validateActualBudgetBackupPolicy(cm object) error {
	if !recoveredNativeObject(cm, "v1", "ConfigMap", "velero") || text(cm, "metadata", "name") != v1BackupPolicy {
		return errActualBudgetBackup
	}
	var live object
	if err := yaml.UnmarshalStrict([]byte(text(cm, "data", "policy.yaml")), &live); err != nil {
		return errActualBudgetBackup
	}
	want := object{
		"version": "v1",
		"volumePolicies": []any{
			object{"conditions": object{"storageClass": []any{"hcloud"}, "pvcLabels": object{
				"backup.platform.devantler.tech/volume-data": "independently-mirrored",
			}}, "action": object{"type": "skip"}},
			object{"conditions": object{"storageClass": []any{"longhorn"}}, "action": object{"type": "fs-backup"}},
			object{"conditions": object{"volumeTypes": []any{"emptyDir"}}, "action": object{"type": "skip"}},
		},
	}
	if !reflect.DeepEqual(live, want) {
		return errActualBudgetBackup
	}
	return nil
}

func actualBudgetTransfer(proof, backup, pod object, claimUID string) bool {
	done, goodDone := nativeObservationUint(value(proof, "status", "progress", "bytesDone"))
	total, goodTotal := nativeObservationUint(value(proof, "status", "progress", "totalBytes"))
	return nativeRestoreUID(claimUID) && seedVeleroIdentity(proof) &&
		text(proof, "apiVersion") == "velero.io/v1" && text(proof, "kind") == "PodVolumeBackup" &&
		controlledBy(proof, backup) && text(proof, "metadata", "labels", "velero.io/pvc-uid") == claimUID &&
		text(proof, "spec", "pod", "uid") == text(pod, "metadata", "uid") &&
		text(proof, "spec", "pod", "name") == text(pod, "metadata", "name") && text(proof, "spec", "pod", "namespace") == actualBudgetNamespace &&
		text(proof, "spec", "volume") == "data" && text(proof, "spec", "backupStorageLocation") == "default" &&
		text(proof, "spec", "uploaderType") == "kopia" && goodDone && goodTotal && done > 0 && total == done &&
		text(proof, "status", "snapshotID") != "" && cleanVeleroResult(proof) == nil
}

func runActualBudgetBackup(ctx context.Context, c client, source, run, digest string) error {
	if c.context != "admin@prod" || !validBenchmarkOwner(run, source) || !retentionDigest.MatchString(digest) {
		return errActualBudgetBackup
	}
	location, err := c.get(ctx, "backupstoragelocations.velero.io", "velero", "default", false)
	if err != nil {
		return errActualBudgetBackup
	}
	schedules, err := c.list(ctx, "schedules.velero.io", "velero")
	if err != nil {
		return errActualBudgetBackup
	}
	locationID, locationSpec, err := verifySeedBackupInputs(location, schedules, time.Now())
	if err != nil {
		return errActualBudgetBackup
	}
	policy, err := c.get(ctx, "configmap", "velero", v1BackupPolicy, false)
	if err != nil || validateActualBudgetBackupPolicy(policy) != nil {
		return errors.New("the reviewed production filesystem backup policy has not been observed")
	}
	pod, claimUID, err := observeActualBudgetBackupSource(ctx, c, digest)
	if err != nil {
		return err
	}
	backups, err := c.list(ctx, "backups.velero.io", "velero")
	if err != nil || !settledActualBudgetBackupWindow(backups) {
		return errors.New("a Velero backup is unfinished or unverified")
	}
	desired := actualBudgetBackup(run)
	created, err := c.create(ctx, source, desired)
	if err != nil || !seedVeleroIdentity(created) ||
		!expectedActualBudgetBackup(created, desired) {
		return errors.New("the recovery backup creation is unconfirmed, preserve the invocation record")
	}
	identity := benchmarkIdentity(created)
	return waitReadyWithin(ctx, 20*time.Minute, func(ctx context.Context) error {
		backup, err := c.get(ctx, "backups.velero.io", "velero", identity.Name, false)
		if err != nil || benchmarkIdentity(backup) != identity || !expectedActualBudgetBackup(backup, desired) {
			return errors.Join(errReadinessTerminal, errActualBudgetBackup)
		}
		if actualBudgetBackupFailed(backup) {
			return errors.Join(errReadinessTerminal, errActualBudgetBackup)
		}
		if err := cleanVeleroResult(backup); err != nil {
			return errActualBudgetBackup
		}

		transfers, err := c.list(ctx, "podvolumebackups.velero.io", "velero")
		if err != nil {
			return errActualBudgetBackup
		}
		matched := 0
		for _, transfer := range transfers {
			if controlledBy(transfer, backup) {
				if !actualBudgetTransfer(transfer, backup, pod, claimUID) {
					return errors.Join(errReadinessTerminal, errActualBudgetBackup)
				}
				matched++
			}
		}
		current, currentUID, err := observeActualBudgetBackupSource(ctx, c, digest)
		if matched != 1 || err != nil || currentUID != claimUID || benchmarkIdentity(current) != benchmarkIdentity(pod) {
			return errActualBudgetBackup
		}
		currentPolicy, err := c.get(ctx, "configmap", "velero", v1BackupPolicy, false)
		if err != nil || benchmarkIdentity(currentPolicy) != benchmarkIdentity(policy) ||
			text(currentPolicy, "metadata", "resourceVersion") != text(policy, "metadata", "resourceVersion") {
			return errors.Join(errReadinessTerminal, errActualBudgetBackup)
		}
		currentLocation, err := c.get(ctx, "backupstoragelocations.velero.io", "velero", "default", false)
		if err != nil {
			return errActualBudgetBackup
		}
		readID, readSpec, err := verifySeedBackupInputs(currentLocation, schedules, time.Now())
		if err != nil || readID != locationID || readSpec != locationSpec {
			return errors.Join(errReadinessTerminal, errActualBudgetBackup)
		}
		return nil
	})
}

func settledActualBudgetBackupWindow(backups []object) bool {
	for _, backup := range backups {
		if !seedVeleroIdentity(backup) || text(backup, "apiVersion") != "velero.io/v1" ||
			text(backup, "kind") != "Backup" {
			return false
		}
		switch text(backup, "status", "phase") {
		case "Completed", "Failed", "FailedValidation", "PartiallyFailed":
		default:
			return false
		}
	}
	return true
}

func expectedActualBudgetBackup(actual, desired object) bool {
	if !seedVeleroIdentity(actual) || text(actual, "apiVersion") != "velero.io/v1" || text(actual, "kind") != "Backup" ||
		text(actual, "metadata", "name") != text(desired, "metadata", "name") ||
		text(actual, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") !=
			text(desired, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") {
		return false
	}
	for name, expected := range valueObject(desired, "spec") {
		if !reflect.DeepEqual(value(actual, "spec", name), expected) {
			return false
		}
	}
	for _, name := range []string{"hooks", "orLabelSelectors", "orderedResources", "volumeSnapshotLocations",
		"includedNamespaceScopedResources", "includedClusterScopedResources"} {
		if value(actual, "spec", name) != nil {
			return false
		}
	}
	return true
}

func actualBudgetBackupFailed(backup object) bool {
	switch text(backup, "status", "phase") {
	case "Failed", "FailedValidation", "PartiallyFailed", "Deleting":
		return true
	default:
		return false
	}
}
