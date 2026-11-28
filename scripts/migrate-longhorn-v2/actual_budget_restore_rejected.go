package main

import (
	"context"
	"errors"
)

type actualBudgetRejectedRestore struct {
	prior      actualBudgetPriorRestore
	restoreUID string
}

func parseActualBudgetRejectedRestore(fields []string, backupRun, currentRun string) *actualBudgetRejectedRestore {
	if len(fields) != 4 || !validBenchmarkOwner(fields[0], fields[1]) || !nativeRestoreUID(fields[2]) ||
		!nativeRestoreUID(fields[3]) || fields[2] == fields[3] || fields[0] == backupRun || fields[0] == currentRun {
		return nil
	}
	return &actualBudgetRejectedRestore{
		prior:      actualBudgetPriorRestore{run: fields[0], source: fields[1], namespaceUID: fields[2]},
		restoreUID: fields[3],
	}
}

func (rejected actualBudgetRejectedRestore) distinctFrom(prepared *actualBudgetPriorRestore) bool {
	return prepared == nil || prepared.run != rejected.prior.run &&
		prepared.namespaceUID != rejected.prior.namespaceUID && prepared.namespaceUID != rejected.restoreUID
}

func observeActualBudgetRejectedRestore(ctx context.Context, c client, opts actualBudgetRestoreConfirmation, run, node string, backup object) (benchmarkResource, error) {
	refused := errors.New("the previous Actual Budget restore was not verified as rejected before allocation")
	rejected := opts.rejected
	if rejected == nil || !rejected.distinctFrom(opts.prior) || !nativeRestoreUID(rejected.restoreUID) {
		return benchmarkResource{}, refused
	}
	prior := rejected.prior
	opts.prior = &prior
	if err := verifyUnstartedActualBudgetOwner(ctx, c, opts, run); err != nil {
		return benchmarkResource{}, err
	}
	assets, err := actualBudgetRestoreAssets(prior.run, opts.backupRun, node)
	if err != nil {
		return benchmarkResource{}, err
	}
	namespace, desiredRestore := assets[0], assets[len(assets)-1]
	name := text(namespace, "metadata", "name")
	valueObject(namespace, "metadata")["annotations"] = object{
		"storage.platform.devantler.tech/recovery-source": prior.source,
		"storage.platform.devantler.tech/backup-uid":      text(backup, "metadata", "uid"),
	}
	observed, err := c.get(ctx, "namespace", "", name, false)
	identity := benchmarkIdentity(observed)
	if err != nil || identity.UID != prior.namespaceUID || !actualBudgetPreparationMatches(observed, namespace) {
		return benchmarkResource{}, refused
	}
	restore, err := c.get(ctx, "restores.velero.io", "velero", name, false)
	if err != nil || !actualBudgetRestoreWasRejected(restore, desiredRestore, rejected.restoreUID) {
		return benchmarkResource{}, refused
	}
	if err = verifyEmptyActualBudgetRestoreAllocation(ctx, c, name, rejected.restoreUID); err != nil {
		return benchmarkResource{}, err
	}
	return identity, nil
}

func actualBudgetRestoreWasRejected(restore, desired object, uid string) bool {
	name := text(desired, "metadata", "name")
	identity := benchmarkResource{Name: name, Namespace: "velero", UID: uid}
	if !nativeRestoreUID(uid) || !actualBudgetRestoreMatches(restore, desired, identity) ||
		text(restore, "status", "phase") != "FailedValidation" {
		return false
	}
	messages, _ := value(restore, "status", "validationErrors").([]any)
	const reason = ": failed to decode yaml data into resource modifiers, err: error unmarshaling JSON: while decoding JSON: json: unknown field \"op\""
	want := "Error in parsing resource modifiers provided in configmap velero/" + name + "-modifiers" + reason
	if len(messages) != 1 || messages[0] != want {
		return false
	}
	for _, field := range []string{"errors", "warnings", "restoreItemOperationsAttempted", "restoreItemOperationsCompleted", "restoreItemOperationsFailed"} {
		if count := value(restore, "status", field); count != nil && count != float64(0) {
			return false
		}
	}
	return value(restore, "status", "progress") == nil
}
