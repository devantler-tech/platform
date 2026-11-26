package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
)

const csiReclaimFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

func (r nativeRestoreReclaim) qualifyStorageReclaim(ctx context.Context, policies []object) (nativeRestoreDetachedObservation, nativeRestoreFenceProof, error) {
	var detached nativeRestoreDetachedObservation
	if err := waitReady(ctx, func(ctx context.Context) error {
		actual, err := r.inputs.detached(ctx, r.c)
		if err == nil {
			detached = actual
		}
		return err
	}); err != nil {
		return detached, nativeRestoreFenceProof{}, err
	}
	proof, err := r.proveFences(ctx, policies, detached)
	if err != nil {
		return detached, proof, err
	}
	if err := r.inputs.retainedSeed(ctx, r.c); err != nil {
		return detached, proof, err
	}
	return detached, proof, nil
}

func (r *nativeRestoreReclaim) reclaimStorage(ctx context.Context, policies []object, detached nativeRestoreDetachedObservation, proof nativeRestoreFenceProof) error {
	if err := r.inputs.retainedSeed(ctx, r.c); err != nil {
		return err
	}
	if err := r.advance(ctx, "reclaim-policy-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	// A competing reference could have been admitted before the fences took
	// effect. Read the complete inventory again under the proved fences.
	fresh, err := r.inputs.detached(ctx, r.c)
	if err != nil || !reflect.DeepEqual(value(fresh.PV, "spec"), value(detached.PV, "spec")) ||
		!reflect.DeepEqual(value(fresh.Claim, "spec"), value(detached.Claim, "spec")) {
		return errNativeRestoreEvidence
	}
	detached = fresh
	if err := r.rebindFences(ctx, policies, proof); err != nil {
		return err
	}
	if err := r.c.patch(ctx, "persistentvolume", "", detached.PV,
		object{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		object{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"}); err != nil {
		return errors.New("the exact canary PV reclaim change was not confirmed")
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		_, err := r.verifyReclaimReady(ctx, detached)
		return err
	}); err != nil {
		return err
	}
	if err := r.advance(ctx, "claim-removal-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	claim, err := r.verifyReclaimReady(ctx, detached)
	if err != nil {
		return err
	}
	if err := r.rebindFences(ctx, policies, proof); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.progress.Source, "/api/v1/namespaces/"+benchmarkNamespace+"/persistentvolumeclaims/fio-data", claim); err != nil {
		return errors.New("the original canary claim removal was not confirmed")
	}
	return r.advance(ctx, "claim-removal-confirmed", "", benchmarkFenceReceipt{})
}

func (r nativeRestoreReclaim) verifyReclaimReady(ctx context.Context, before nativeRestoreDetachedObservation) (object, error) {
	state := r.inputs.state
	observed, err := readNativeRestoreDetached(ctx, r.c, state)
	if err != nil || !nativeRestoreCSIReclaimReady(before, observed) {
		return nil, errNativeRestoreEvidence
	}
	// Quiescence still checks the original retained binding. The one verified
	// reclaim change is already accounted for and no other field is normalized.
	pv := maps.Clone(observed.PV)
	spec := maps.Clone(valueObject(pv, "spec"))
	spec["persistentVolumeReclaimPolicy"] = "Retain"
	pv["spec"] = spec
	observed.PV = pv
	if err := r.inputs.verifyDetachedReferences(ctx, r.c, observed); err != nil {
		return nil, err
	}
	return observed.Claim, nil
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
		text(observed.Claim, "metadata", "resourceVersion") != "" &&
		reflect.DeepEqual(value(observed.Claim, "spec"), value(before.Claim, "spec")) &&
		text(observed.PV, "status", "phase") == "Bound" &&
		slices.Contains(finalizers, any(csiReclaimFinalizer)) && reflect.DeepEqual(value(observed.PV, "spec"), spec)
}
