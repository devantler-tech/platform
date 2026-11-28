package main

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func (j *openBaoPairJournal) copyStagedPair(ctx context.Context, token string, qualified []cnpgRecoveryDiskBinding) error {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() || !r.Evicting {
		return errOpenBaoProviderStorage
	}
	if err := j.waitForOfflinePair(ctx, token); err != nil {
		return err
	}
	policy, gate, pod, err := j.prepareCopyAssets(ctx)
	if err != nil {
		return err
	}
	isolation, err := j.waitAndReleaseCopyGate(ctx, pod, policy, gate, token, qualified)
	if err != nil {
		return err
	}
	if err := j.recordVerifiedCopy(ctx, pod, policy, isolation, token, qualified); err != nil {
		return err
	}
	if err := removeScratch(ctx, j.client, r.Plan.Source, "pod", openBaoNamespace,
		"/api/v1/namespaces/"+openBaoNamespace+"/pods/"+benchmarkIdentity(pod).Name, pod); err != nil {
		return err
	}
	return j.waitForOfflinePair(ctx, token)
}

func (j openBaoPairJournal) waitForOfflinePair(ctx context.Context, token string) error {
	return waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		storage, err := j.readCopyStorage(ctx, token, 2, false)
		if err != nil {
			return err
		}
		if err := verifyOpenBaoDetachedSources(j.record.Plan.Original, storage.claims, storage.pvs,
			storage.pods, storage.tickets, storage.provider[:]); err != nil {
			return err
		}
		return verifyOpenBaoDestinationsDetached(ctx, j.client, j.record.Destinations)
	})
}

// Create the deny policy and the closed gate before a Pod can mount the pair.
// An ambiguous create stops at the journaled intent for reconciled recovery.
func (j *openBaoPairJournal) prepareCopyAssets(ctx context.Context) (policy, gate, pod object, err error) {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() || !r.Evicting ||
		r.CopyPending != "" || r.CopyAssets != ([3]benchmarkResource{}) {
		return nil, nil, nil, errOpenBaoProviderStorage
	}
	plan := r.Plan
	var created [3]object
	for index, asset := range desiredOpenBaoCopyAssets(plan) {
		next := j.record
		next.CopyPending = openBaoCopyAssetKinds[index]
		if err := j.advance(ctx, next); err != nil {
			return nil, nil, nil, err
		}
		observed, err := j.client.create(ctx, plan.Source, asset)
		if err != nil || !matchOpenBaoNewCopyAsset(index, observed, asset, plan) {
			return nil, nil, nil, errors.Join(err, errOpenBaoProviderStorage)
		}
		next.CopyPending, next.CopyAssets[index] = "", benchmarkIdentity(observed)
		if err := j.advance(ctx, next); err != nil {
			return nil, nil, nil, err
		}
		created[index] = observed
	}
	return created[0], created[1], created[2], nil
}

func desiredOpenBaoCopyAssets(plan openBaoPairPlan) [3]object {
	return [3]object{
		openBaoCopyIsolation.denyPolicy(plan.Run, plan.Source, openBaoCopyFenceOwner),
		openBaoCopyGate(plan.Run, plan.Source),
		plan.copyPod(),
	}
}

// Recover an interrupted create without issuing it again. A recorded Pod, if
// present, is removed by the normal rollback path before permitting the member.
func (j *openBaoPairJournal) reconcilePendingCopy(ctx context.Context) error {
	r := j.record
	if r.CopyPending == "" {
		return nil
	}
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() || !r.Evicting {
		return errOpenBaoProviderStorage
	}
	index, _ := r.copyProgress()
	resource := [3]string{"ciliumnetworkpolicies.cilium.io", "configmap", "pod"}[index]
	desired := desiredOpenBaoCopyAssets(r.Plan)[index]
	observed, err := j.client.get(ctx, resource, openBaoNamespace, text(desired, "metadata", "name"), true)
	if err != nil {
		return err
	}
	if observed != nil {
		matched := matchOpenBaoNewCopyAsset(index, observed, desired, r.Plan)
		if index == 2 {
			matched = j.client.matchBoundOpenBaoVolumePod(ctx, observed, observed, desired, r.Plan.Original.Node)
		}
		if !matched {
			return errOpenBaoProviderStorage
		}
		r.CopyAssets[index] = benchmarkIdentity(observed)
	}
	r.CopyPending = ""
	return j.advance(ctx, r)
}

func matchOpenBaoNewCopyAsset(index int, observed, desired object, plan openBaoPairPlan) bool {
	switch index {
	case 0:
		return recoveredNativeObject(observed, "cilium.io/v2", "CiliumNetworkPolicy", openBaoNamespace) &&
			text(observed, "metadata", "name") == text(desired, "metadata", "name") &&
			text(observed, "metadata", "annotations", openBaoCopyFenceOwner) == plan.Run+":"+plan.Source &&
			reflect.DeepEqual(value(observed, "spec"), value(desired, "spec")) && value(observed, "specs") == nil
	case 1:
		return matchOpenBaoCopyGate(observed, observed, plan, "hold") &&
			reflect.DeepEqual(value(observed, "data"), value(desired, "data"))
	case 2:
		return matchStorageCopyVolumePod(observed, observed, desired, openBaoNamespace, openBaoCopyFenceOwner)
	default:
		return false
	}
}
