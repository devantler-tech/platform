package main

import (
	"context"
	"os"
	"time"
)

// This read-only graph observation grants no mutation. The executor must also
// rebind its original journals, backup, Velero epoch, prepared host geometry,
// and V2 runtime/capacity inputs before and after any payload operation.
func observeNativeRestoreAttached(ctx context.Context, c client, record benchmarkRecord, seed canarySeedState, state nativeRestoreState, config []byte, helperDigest string, managers map[string]nativeRestoreV2ManagerIdentity) (nativeRestoreAttachmentReceipt, error) {
	fail := func() (nativeRestoreAttachmentReceipt, error) {
		return nativeRestoreAttachmentReceipt{}, errNativeRestoreEvidence
	}
	if !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed || authorizeNativeRestore(os.Getenv) != nil ||
		state.Source != os.Getenv("GITHUB_SHA") || state.Run != os.Getenv("GITHUB_RUN_ID") || state.validate(record, seed) != nil ||
		!nativeRestoreAttachedManagerRoster(state.Targets, managers, nativeRestoreReceiptUIDs(record, seed, state)) {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if sourceCurrent(ctx, c.run, state.Source) != nil {
		return fail()
	}
	before, err := readNativeRestoreAttached(ctx, c, record, seed, state, config, helperDigest, managers)
	if err != nil {
		return fail()
	}
	after, err := readNativeRestoreAttached(ctx, c, record, seed, state, config, helperDigest, managers)
	if err != nil || before != after || sourceCurrent(ctx, c.run, state.Source) != nil || !commandContextActive(ctx) {
		return fail()
	}
	return after, nil
}

func readNativeRestoreAttached(ctx context.Context, c client, record benchmarkRecord, seed canarySeedState, state nativeRestoreState, config []byte, helperDigest string, managers map[string]nativeRestoreV2ManagerIdentity) (nativeRestoreAttachmentReceipt, error) {
	observation := nativeRestoreAttachedObservation{Config: config, HelperDigest: helperDigest, Managers: managers}
	podKind := "FirstPod"
	if nativeRestorePhaseIndex(state.Phase) >= nativeRestorePhaseIndex("reattach-owned") {
		podKind = "SecondPod"
	}
	for _, query := range []struct {
		kind, receipt string
		result        *object
	}{
		{"persistentvolumeclaim", "PersistentVolumeClaim", &observation.Claim},
		{"storageclass", "StorageClass", &observation.Class},
		{"persistentvolume", "PersistentVolume", &observation.PV},
		{"pod", podKind, &observation.Pod},
	} {
		identity := state.Resources[query.receipt]
		actual, err := c.get(ctx, query.kind, identity.Namespace, identity.Name, false)
		if err != nil {
			return nativeRestoreAttachmentReceipt{}, errNativeRestoreEvidence
		}
		*query.result = actual
	}
	var err error
	observation.Inventory, err = c.snapshot(ctx)
	if err != nil {
		return nativeRestoreAttachmentReceipt{}, errNativeRestoreEvidence
	}
	observation.Frontends, err = c.list(ctx, "enginefrontends.longhorn.io", "longhorn-system")
	if err != nil {
		return nativeRestoreAttachmentReceipt{}, errNativeRestoreEvidence
	}
	if err := rebindNativeRestoreAttachedManagers(ctx, c, state.Targets, observation.Inventory.storageNodes, managers); err != nil {
		return nativeRestoreAttachmentReceipt{}, err
	}
	return verifyNativeRestoreAttached(record, seed, state, observation)
}

func rebindNativeRestoreAttachedManagers(ctx context.Context, c client, targets []nativeRestoreTarget, storage []object, managers map[string]nativeRestoreV2ManagerIdentity) error {
	nodes, err := indexed(storage)
	if err != nil {
		return errNativeRestoreEvidence
	}
	for _, target := range targets {
		expected := managers[target.Name]
		manager, err := c.get(ctx, "instancemanagers.longhorn.io", "longhorn-system", expected.Pod, false)
		if err != nil {
			return errNativeRestoreEvidence
		}
		pod, err := c.get(ctx, "pod", "longhorn-system", expected.Pod, false)
		if err != nil {
			return errNativeRestoreEvidence
		}
		actual, err := qualifyNativeRestoreV2ManagerIdentity(target, nodes[target.Name], manager, pod)
		if err != nil || actual != expected {
			return errNativeRestoreEvidence
		}
	}
	return nil
}
