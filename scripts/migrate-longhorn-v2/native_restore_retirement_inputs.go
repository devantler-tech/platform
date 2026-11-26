package main

import (
	"context"
	"os"
	"reflect"
)

type nativeRestoreReclaimInputs struct {
	journal  benchmarkResource
	record   benchmarkRecord
	seed     canarySeedState
	state    nativeRestoreState
	executor string
}

func readNativeRestoreReclaimInputs(ctx context.Context, c client) (nativeRestoreReclaimInputs, error) {
	if _, err := authorizedNativeRestoreReclaimRun(os.Getenv); err != nil || !commandContextActive(ctx) ||
		c.run == nil || c.context != "admin@prod" || c.nativeSeed {
		return nativeRestoreReclaimInputs{}, errNativeRestoreEvidence
	}
	if err := sourceCurrent(ctx, c.run, os.Getenv("GITHUB_SHA")); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, c)
	executor := text(journal, "metadata", "annotations", nativeRestoreExecutorAnnotation)
	if err != nil || !nativeRestoreExecutorMatches(journal, executor) || nativeRestoreReclaimOwnerSucceeded(ctx, c, state, executor) != nil {
		return nativeRestoreReclaimInputs{}, errNativeRestoreEvidence
	}
	inputs := nativeRestoreReclaimInputs{journal: benchmarkIdentity(journal), record: record, seed: seed, state: state, executor: executor}
	if _, err := observeBenchmarkWorkspace(ctx, c, record.NamespaceUID); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	if err := observeNativeRestoreSourceAbsent(ctx, c, record, seed); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	if err := inputs.retainedSeed(ctx, c); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	if _, _, _, err := inputs.veleroArtifacts(ctx, c); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	if err := inputs.rebind(ctx, c); err != nil {
		return nativeRestoreReclaimInputs{}, err
	}
	return inputs, nil
}

func (inputs nativeRestoreReclaimInputs) retainedSeed(ctx context.Context, c client) error {
	pod := inputs.record.Resources["Pod"]
	archived := object{"metadata": object{"name": pod.Name, "namespace": pod.Namespace, "uid": pod.UID}}
	reader := nativeSeedRun{c: c, record: inputs.record, state: inputs.seed, created: map[string]object{"Pod": archived}}
	return reader.retainedBackup(ctx)
}

func (inputs nativeRestoreReclaimInputs) rebind(ctx context.Context, c client) error {
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, c)
	if err != nil || benchmarkIdentity(journal) != inputs.journal || !reflect.DeepEqual(record, inputs.record) ||
		seed != inputs.seed || !reflect.DeepEqual(state, inputs.state) || !nativeRestoreExecutorMatches(journal, inputs.executor) {
		return errNativeRestoreEvidence
	}
	return nil
}

func (inputs nativeRestoreReclaimInputs) veleroArtifacts(ctx context.Context, c client) (object, object, object, error) {
	state := inputs.state
	get := func(kind, key string) (object, error) {
		id := state.Resources[key]
		return c.get(ctx, kind, id.Namespace, id.Name, false)
	}
	restore, err := get("restores.velero.io", "Restore")
	if err != nil {
		return nil, nil, nil, err
	}
	modifier, err := get("configmaps", "ConfigMap")
	if err != nil {
		return nil, nil, nil, err
	}
	transfers, err := c.list(ctx, "podvolumerestores.velero.io", "velero")
	if err != nil {
		return nil, nil, nil, err
	}
	repositories, err := c.list(ctx, "backuprepositories.velero.io", "velero")
	if err != nil || verifyNativeRestoreRetirementVelero(inputs.record, inputs.seed, state, restore, modifier, transfers, repositories) != nil {
		return nil, nil, nil, errNativeRestoreEvidence
	}
	for _, transfer := range transfers {
		if benchmarkIdentity(transfer) == state.Resources["PodVolumeRestore"] {
			return restore, modifier, transfer, nil
		}
	}
	return nil, nil, nil, errNativeRestoreEvidence
}

func (inputs nativeRestoreReclaimInputs) detached(ctx context.Context, c client) (nativeRestoreDetachedObservation, error) {
	actual, err := readNativeRestoreDetached(ctx, c, inputs.state)
	if err != nil {
		return actual, err
	}
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return actual, err
	}
	claims, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return actual, err
	}
	return actual, verifyNativeRestoreReclaim(inputs.record, inputs.seed, inputs.state, actual, pvs, claims)
}
