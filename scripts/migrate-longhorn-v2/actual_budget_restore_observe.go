package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"
)

func verifyActualBudgetRestoreClass(ctx context.Context, c client) error {
	sc, err := c.get(ctx, "storageclass", "", "longhorn-v2", false)
	parameters := object{"backupTargetName": "default", "dataEngine": "v2", "dataLocality": "disabled",
		"fsType": "ext4", "numberOfReplicas": "3", "staleReplicaTimeout": "30", "unmapMarkSnapChainRemoved": "disabled"}
	if err != nil || !recoveredNativeObject(sc, "storage.k8s.io/v1", "StorageClass", "") ||
		text(sc, "metadata", "name") != "longhorn-v2" || text(sc, "provisioner") != "driver.longhorn.io" ||
		text(sc, "reclaimPolicy") != "Retain" || text(sc, "volumeBindingMode") != "WaitForFirstConsumer" ||
		!reflect.DeepEqual(value(sc, "parameters"), parameters) {
		return errActualBudgetBackup
	}
	return nil
}

func actualBudgetRestoredClaim(ctx context.Context, c client, restore object, originalClaimUID string) (object, error) {
	ns := text(restore, "metadata", "name")
	claims, err := c.list(ctx, "persistentvolumeclaims", ns)
	if err != nil || len(claims) != 1 {
		return nil, errActualBudgetBackup
	}
	claim := claims[0]
	uid := text(claim, "metadata", "uid")
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", ns) || !nativeRestoreUID(uid) || uid == originalClaimUID ||
		text(claim, "metadata", "name") != actualBudgetClaim || text(claim, "metadata", "labels", "velero.io/restore-name") != ns ||
		text(claim, "metadata", "labels", "velero.io/backup-name") != text(restore, "spec", "backupName") ||
		text(claim, "spec", "storageClassName") != "longhorn-v2" || text(claim, "status", "phase") != "Bound" ||
		text(claim, "spec", "resources", "requests", "storage") != actualBudgetRestoreSize ||
		text(claim, "status", "capacity", "storage") != actualBudgetRestoreSize || !providerRWOFilesystem(claim) {
		return nil, errActualBudgetBackup
	}
	return claim, nil
}

func actualBudgetProbeReady(pod, initial, desired object) bool {
	if benchmarkIdentity(pod) != benchmarkIdentity(initial) || !recoveredNativeObject(pod, "v1", "Pod", text(initial, "metadata", "namespace")) ||
		text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
		value(pod, "spec", "hostUsers") != false || value(pod, "spec", "automountServiceAccountToken") != false ||
		value(pod, "spec", "initContainers") != nil || value(pod, "spec", "ephemeralContainers") != nil ||
		value(pod, "spec", "hostNetwork") == true || value(pod, "spec", "hostPID") == true || value(pod, "spec", "hostIPC") == true ||
		!reflect.DeepEqual(value(pod, "spec", "volumes"), value(desired, "spec", "volumes")) {
		return false
	}
	containers, _ := value(pod, "spec", "containers").([]any)
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if len(containers) != 1 || len(statuses) != 1 {
		return false
	}
	container, _ := containers[0].(object)
	status, _ := statuses[0].(object)
	original := value(desired, "spec", "containers").([]any)[0].(object)
	for _, key := range []string{"name", "image", "command", "workingDir", "env", "securityContext", "volumeMounts"} {
		if !reflect.DeepEqual(value(container, key), value(original, key)) {
			return false
		}
	}
	_, digest, _ := strings.Cut(actualBudgetProofImage, "@")
	return text(status, "name") == "verify" && value(status, "ready") == true && value(status, "restartCount") == float64(0) &&
		strings.HasSuffix(text(status, "imageID"), "@"+digest)
}

func verifyActualBudgetRestoreBackend(ctx context.Context, c client, claim, pod object, disks []cnpgRecoveryDiskBinding) error {
	name := text(claim, "spec", "volumeName")
	if !strings.HasPrefix(name, "pvc-") || !nativeRestoreUID(strings.TrimPrefix(name, "pvc-")) {
		return errActualBudgetBackup
	}
	pv, err := c.get(ctx, "persistentvolume", "", name, false)
	if err != nil {
		return errActualBudgetBackup
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", name, false)
	if err != nil {
		return errActualBudgetBackup
	}
	engines, err := c.list(ctx, "engines.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(engines, "longhorn.io/v1beta2", "Engine", "longhorn-system") {
		return errActualBudgetBackup
	}
	var engine object
	for _, candidate := range engines {
		if recoveredVolumeAssociated(candidate, volume) {
			if engine != nil {
				return errActualBudgetBackup
			}
			engine = candidate
		}
	}
	node := text(pod, "spec", "nodeName")
	if node == "" || !recoveredPVBackend(claim, pv, volume, engine, text(claim, "metadata", "namespace"), name, actualBudgetRestoreSize, actualBudgetRestoreBytes, node) {
		return errActualBudgetBackup
	}
	replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(replicas, "longhorn.io/v1beta2", "Replica", "longhorn-system") {
		return errActualBudgetBackup
	}
	_, err = recoveredNativeCopies(disks, volume, engine, replicas, actualBudgetRestoreBytes)
	return err
}

func verifyActualBudgetRestoredPayload(ctx context.Context, c client, source, run, digest, originalClaimUID string, restore object, disks []cnpgRecoveryDiskBinding) error {
	ns := text(restore, "metadata", "name")
	claim, err := actualBudgetRestoredClaim(ctx, c, restore, originalClaimUID)
	if err != nil {
		return errors.New("an isolated bound V2 claim is unverified")
	}
	pods, err := c.list(ctx, "pods", ns)
	if err != nil || len(pods) != 0 {
		return errors.New("a foreign workload occupies the restore scope")
	}
	policies, err := c.list(ctx, "networkpolicies.networking.k8s.io", ns)
	if err != nil || len(policies) != 1 || text(policies[0], "metadata", "name") != "restore-isolation" ||
		text(policies[0], "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != run ||
		!reflect.DeepEqual(value(policies[0], "spec"), object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}) {
		return errors.New("the content verifier namespace isolation is unverified")
	}
	desired := actualBudgetProofPod(ns, run)
	created, err := c.create(ctx, source, desired)
	if err != nil {
		return errors.New("content verifier creation is unconfirmed")
	}
	name := text(created, "metadata", "name")
	err = waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		pod, err := c.get(ctx, "pod", ns, name, false)
		if err != nil || benchmarkIdentity(pod) != benchmarkIdentity(created) || text(pod, "status", "phase") == "Failed" {
			return errors.Join(errActualBudgetBackup, errReadinessTerminal)
		}
		if !actualBudgetProbeReady(pod, created, desired) {
			return errActualBudgetBackup
		}
		current, err := actualBudgetRestoredClaim(ctx, c, restore, originalClaimUID)
		if err != nil || benchmarkIdentity(current) != benchmarkIdentity(claim) || text(current, "spec", "volumeName") != text(claim, "spec", "volumeName") {
			return errors.Join(errActualBudgetBackup, errReadinessTerminal)
		}
		if err = verifyActualBudgetRestoreBackend(ctx, c, current, pod, disks); err != nil {
			return err
		}
		log, err := c.kubectl(ctx, nil, "-n", ns, "logs", name, "-c", "verify", "--limit-bytes=4096")
		if err != nil || strings.TrimSpace(string(log)) != "ACTUAL_BUDGET_RESTORED_CONTENT=VERIFIED" {
			return errActualBudgetBackup
		}
		return nil
	})
	if err != nil {
		return errors.New("the payload or three native V2 replicas were not verified")
	}
	if _, uid, err := observeActualBudgetBackupSource(ctx, c, digest); err != nil || uid != originalClaimUID {
		return errors.New("the serving claim changed during the restore")
	}
	if err = removeScratch(ctx, c, source, "pod", ns, "/api/v1/namespaces/"+ns+"/pods/"+name, created); err != nil {
		return err
	}
	retained, err := actualBudgetRestoredClaim(ctx, c, restore, originalClaimUID)
	if err != nil || benchmarkIdentity(retained) != benchmarkIdentity(claim) || text(retained, "spec", "volumeName") != text(claim, "spec", "volumeName") {
		return errors.New("the verified V2 claim was not retained after retiring the reader")
	}
	return nil
}
