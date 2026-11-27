package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	actualBudgetRestoreOperation = "restore-actual-budget-v2"
	actualBudgetRestoreSize      = "10Gi"
	actualBudgetRestoreBytes     = "10737418240"
	actualBudgetRestorePrefix    = "actual-budget-recovery-"
)

type actualBudgetRestoreConfirmation struct {
	backupRun, backupSource, sourceClaimDigest string
}

func authorizeActualBudgetRestore(getenv func(string) string) (actualBudgetRestoreConfirmation, error) {
	confirm := getenv("LONGHORN_V2_CONFIRM")
	fields := strings.Split(confirm, ":")
	if getenv("LONGHORN_V2_OPERATION") != actualBudgetRestoreOperation || getenv("LONGHORN_V2_NODE") != "" ||
		getenv("LONGHORN_V2_DATABASE") != "" || len(fields) != 4 || fields[0] != actualBudgetRestoreOperation ||
		!validBenchmarkOwner(fields[1], fields[2]) || fields[1] == getenv("GITHUB_RUN_ID") || !retentionDigest.MatchString(fields[3]) {
		return actualBudgetRestoreConfirmation{}, errActualBudgetBackup
	}
	return actualBudgetRestoreConfirmation{fields[1], fields[2], fields[3]}, authorizeWorkflow(getenv, confirm)
}

// Match the source claim and the completed native upload. Only the immutable
// backup snapshot is used by the isolated restore, never a live filesystem copy.
func readActualBudgetRestoreSeed(ctx context.Context, c client, backupRun, claimUID string) (object, object, error) {
	desired := actualBudgetBackup(backupRun)
	backup, err := c.get(ctx, "backups.velero.io", "velero", text(desired, "metadata", "name"), false)
	if err != nil || !expectedActualBudgetBackup(backup, desired) || cleanVeleroResult(backup) != nil {
		return nil, nil, errActualBudgetBackup
	}
	expires, err := time.Parse(time.RFC3339, text(backup, "status", "expiration"))
	if err != nil || time.Until(expires) < time.Hour {
		return nil, nil, errActualBudgetBackup
	}
	transfers, err := c.list(ctx, "datauploads.velero.io", "velero")
	if err != nil {
		return nil, nil, errActualBudgetBackup
	}
	var upload object
	for _, transfer := range transfers {
		if controlledBy(transfer, backup) {
			if upload != nil || !actualBudgetTransfer(transfer, backup, claimUID) {
				return nil, nil, errActualBudgetBackup
			}
			upload = transfer
		}
	}
	if upload == nil {
		return nil, nil, errActualBudgetBackup
	}
	return backup, upload, nil
}

func actualBudgetRestoreScopeClear(ctx context.Context, c client) bool {
	namespaces, err := c.list(ctx, "namespace", "")
	if err != nil || len(namespaces) == 0 {
		return false
	}
	for _, namespace := range namespaces {
		if text(namespace, "metadata", "name") == "" || strings.HasPrefix(text(namespace, "metadata", "name"), actualBudgetRestorePrefix) {
			return false
		}
	}
	volumes, err := c.list(ctx, "persistentvolumes", "")
	if err != nil || len(volumes) == 0 {
		return false
	}
	for _, pv := range volumes {
		if strings.HasPrefix(text(pv, "spec", "claimRef", "namespace"), actualBudgetRestorePrefix) {
			return false
		}
	}
	return true
}

func actualBudgetRestoreMatches(actual, desired object, identity benchmarkResource) bool {
	if benchmarkIdentity(actual) != identity || !seedVeleroIdentity(actual) || text(actual, "apiVersion") != "velero.io/v1" ||
		text(actual, "kind") != "Restore" || text(actual, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") !=
		text(desired, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") {
		return false
	}
	for key, expected := range valueObject(desired, "spec") {
		actualValue := value(actual, "spec", key)
		if key == "excludedResources" && restoredResourceExclusionsMatch(actualValue, expected) {
			continue
		}
		if !reflect.DeepEqual(actualValue, expected) {
			return false
		}
	}
	for _, key := range []string{"hooks", "orLabelSelectors", "restoreStatus", "uploaderConfig", "namespaceMappingOverride"} {
		if value(actual, "spec", key) != nil {
			return false
		}
	}
	return true
}

func waitActualBudgetRestore(ctx context.Context, c client, created, desired, backup, upload object) error {
	id := benchmarkIdentity(created)
	return waitReadyWithin(ctx, 24*time.Minute, func(ctx context.Context) error {
		actual, err := c.get(ctx, "restores.velero.io", id.Namespace, id.Name, false)
		if err != nil || !actualBudgetRestoreMatches(actual, desired, id) {
			return errors.Join(errActualBudgetBackup, errReadinessTerminal)
		}
		switch text(actual, "status", "phase") {
		case "Failed", "FailedValidation", "PartiallyFailed":
			return errors.Join(errActualBudgetBackup, errReadinessTerminal)
		case "Completed":
			transfers, err := c.list(ctx, "datadownloads.velero.io", "velero")
			if err != nil {
				return errActualBudgetBackup
			}
			return verifyActualBudgetRestoreDownload(actual, transfers, id.Name, text(backup, "metadata", "name"), text(upload, "status", "snapshotID"))
		default:
			return errActualBudgetBackup
		}
	})
}

func runActualBudgetRestore(ctx context.Context, c client, source, run string, opts actualBudgetRestoreConfirmation) error {
	assets, err := actualBudgetRestoreAssets(run, opts.backupRun)
	if err != nil || c.context != "admin@prod" || !validBenchmarkOwner(run, source) ||
		!validBenchmarkOwner(opts.backupRun, opts.backupSource) || !retentionDigest.MatchString(opts.sourceClaimDigest) {
		return errActualBudgetBackup
	}
	producer, err := previousMigrationRun(ctx, c, opts.backupRun, opts.backupSource)
	if err != nil || text(producer, "path") != ".github/workflows/prepare-longhorn-v2-node.yaml" || text(producer, "conclusion") != "success" {
		return errors.New("the original protected backup attempt is unverified")
	}
	_, originalClaimUID, err := observeActualBudgetBackupSource(ctx, c, opts.sourceClaimDigest)
	if err != nil {
		return errors.New("the original serving claim is unverified")
	}
	backup, upload, err := readActualBudgetRestoreSeed(ctx, c, opts.backupRun, originalClaimUID)
	if err != nil {
		return errors.New("the original offsite snapshot is unverified")
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	storage, err := activation.recoveryClaimStorage(ctx, actualBudgetRestoreSize)
	if err != nil {
		return err
	}
	if err = verifyActualBudgetRestoreClass(ctx, c); err != nil || !actualBudgetRestoreScopeClear(ctx, c) {
		return errors.New("the V2 storage class or isolated restore scope is unverified")
	}
	var restore object
	for _, asset := range assets {
		restore, err = c.create(ctx, source, asset)
		if err != nil {
			return errors.New("restore asset creation is unconfirmed, preserve the existing restore scope for recovery")
		}
		if text(asset, "kind") == "NetworkPolicy" && !reflect.DeepEqual(value(restore, "spec"), value(asset, "spec")) {
			return errors.New("restore namespace isolation is unverified")
		}
	}
	if err = waitActualBudgetRestore(ctx, c, restore, assets[len(assets)-1], backup, upload); err != nil {
		return fmt.Errorf("the isolated data transfer is unverified: %w", err)
	}
	return verifyActualBudgetRestoredPayload(ctx, c, source, run, opts.sourceClaimDigest, originalClaimUID, restore, storage.Disks)
}
