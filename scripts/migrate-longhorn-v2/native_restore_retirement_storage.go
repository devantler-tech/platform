package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
)

const csiReclaimFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

func (r *nativeRestoreReclaim) reclaimStorage(ctx context.Context, policies []object) error {
	var detached nativeRestoreDetachedObservation
	if err := waitReady(ctx, func(ctx context.Context) error {
		actual, err := r.inputs.detached(ctx, r.c)
		if err == nil {
			detached = actual
		}
		return err
	}); err != nil {
		return err
	}
	if err := r.proveFences(ctx, policies, detached); err != nil {
		return err
	}
	if err := r.inputs.retainedSeed(ctx, r.c); err != nil {
		return err
	}
	if err := r.advance(ctx, "reclaim-policy-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if err := r.rebindFences(ctx, policies); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.patch(ctx, "persistentvolume", "", detached.PV,
		object{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		object{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"}); err != nil {
		return errors.New("the exact canary PV reclaim change was not confirmed")
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		return r.verifyReclaimReady(ctx, detached)
	}); err != nil {
		return err
	}
	if err := r.advance(ctx, "claim-removal-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if err := r.rebindFences(ctx, policies); err != nil {
		return err
	}
	if err := r.verifyReclaimReady(ctx, detached); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.progress.Source, "/api/v1/namespaces/"+benchmarkNamespace+"/persistentvolumeclaims/fio-data", detached.Claim); err != nil {
		return errors.New("the original canary claim removal was not confirmed")
	}
	return r.advance(ctx, "claim-removal-confirmed", "", benchmarkFenceReceipt{})
}

func (r nativeRestoreReclaim) verifyReclaimReady(ctx context.Context, before nativeRestoreDetachedObservation) error {
	state := r.inputs.state
	observed, err := readNativeRestoreDetached(ctx, r.c, state)
	if err != nil || !nativeRestoreCSIReclaimReady(before, observed) {
		return errNativeRestoreEvidence
	}
	// Quiescence still checks the original retained binding. The one verified
	// reclaim change is already accounted for and no other field is normalized.
	pv := maps.Clone(observed.PV)
	spec := maps.Clone(valueObject(pv, "spec"))
	spec["persistentVolumeReclaimPolicy"] = "Retain"
	pv["spec"] = spec
	observed.PV = pv
	return verifyNativeRestoreQuiescence(r.inputs.record, r.inputs.seed, state, observed)
}

func nativeRestoreCSIReclaimReady(before, observed nativeRestoreDetachedObservation) bool {
	finalizers, _ := value(observed.PV, "metadata", "finalizers").([]any)
	spec := maps.Clone(valueObject(before.PV, "spec"))
	if spec == nil {
		return false
	}
	spec["persistentVolumeReclaimPolicy"] = "Delete"
	return benchmarkIdentity(observed.PV) == benchmarkIdentity(before.PV) &&
		benchmarkIdentity(observed.Claim) == benchmarkIdentity(before.Claim) &&
		text(observed.Claim, "metadata", "resourceVersion") == text(before.Claim, "metadata", "resourceVersion") &&
		text(observed.PV, "status", "phase") == "Bound" &&
		slices.Contains(finalizers, any(csiReclaimFinalizer)) && reflect.DeepEqual(value(observed.PV, "spec"), spec)
}
