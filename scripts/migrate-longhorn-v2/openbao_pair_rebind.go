package main

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func (j *openBaoPairJournal) rebindCopiedClaims(ctx context.Context, token string) error {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairSourcesReleased || !r.valid() {
		return errOpenBaoProviderStorage
	}
	if err := j.proveAdmissionFences(ctx); err != nil {
		return err
	}
	for index := range r.Destinations {
		if err := j.recordSuccessorClaim(ctx, token, index); err != nil {
			return err
		}
		if err := j.rebindSuccessorClaim(ctx, token, index); err != nil {
			return err
		}
	}
	pair, err := j.readRebindPair(ctx, token)
	if err != nil {
		return err
	}
	r = j.record
	if err := verifyOpenBaoReboundPair(r.Plan.Input.Member, pair, r.Successors); err != nil {
		return err
	}
	r.Phase = openBaoPairRebound
	return j.advance(ctx, r)
}

func (j *openBaoPairJournal) recordSuccessorClaim(ctx context.Context, token string, index int) error {
	r := j.record
	dest := r.Destinations[index]
	pair, err := j.readRebindPair(ctx, token)
	if err != nil {
		return err
	}
	observed := pair[index].NewClaim
	if r.Successors[index] != (benchmarkResource{}) {
		if benchmarkIdentity(observed) != r.Successors[index] {
			return errOpenBaoProviderStorage
		}
		return nil
	}
	desired := dest.replacementClaim()
	if r.ClaimPending == 0 {
		if observed != nil {
			return errOpenBaoProviderStorage
		}
		r.ClaimPending = index + 1
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	if r.ClaimPending == index+1 && observed == nil {
		observed, err = j.client.create(ctx, r.Plan.Source, desired)
	}
	// On continuation, only the exact owner and prebound destination allowed
	// by the serving claim fence may be recorded for the pending creation.
	if err != nil || r.ClaimPending != index+1 || !matchOpenBaoSuccessorClaim(observed, desired) {
		return errors.Join(err, errOpenBaoProviderStorage)
	}
	r.Successors[index], r.ClaimPending = benchmarkIdentity(observed), 0
	return j.advance(ctx, r)
}

func matchOpenBaoSuccessorClaim(observed, desired object) bool {
	return recoveredNativeObject(observed, "v1", "PersistentVolumeClaim", openBaoNamespace) &&
		text(observed, "metadata", "name") == text(desired, "metadata", "name") &&
		text(observed, "metadata", "annotations", openBaoVolumeRebindOwner) == text(desired, "metadata", "annotations", openBaoVolumeRebindOwner) &&
		value(observed, "metadata", "ownerReferences") == nil && reflect.DeepEqual(value(observed, "spec"), value(desired, "spec"))
}

func (j openBaoPairJournal) rebindSuccessorClaim(ctx context.Context, token string, index int) error {
	r := j.record
	var pair [2]openBaoReboundStorage
	var ops []object
	if err := waitReadyWithin(ctx, 45*time.Second, func(ctx context.Context) error {
		var err error
		pair, err = j.readRebindPair(ctx, token)
		if err != nil {
			return err
		}
		_, ops, err = pair[index].rebindStep(r.Successors[index])
		return err
	}); err != nil {
		return err
	}
	if len(ops) > 0 {
		if _, err := j.read(ctx); err != nil {
			return err
		}
		if err := sourceCurrent(ctx, j.client.run, r.Plan.Source); err != nil {
			return err
		}
		if err := j.client.patch(ctx, "persistentvolume", "", pair[index].Dest, ops...); err != nil {
			return err
		}
	}
	return waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
		current, err := j.readRebindPair(ctx, token)
		if err != nil {
			return err
		}
		bound, _, err := current[index].rebindStep(r.Successors[index])
		if err != nil || !bound {
			return errOpenBaoProviderStorage
		}
		return nil
	})
}

func (j openBaoPairJournal) readRebindPair(ctx context.Context, token string) ([2]openBaoReboundStorage, error) {
	var pair [2]openBaoReboundStorage
	r := j.record
	storage, err := j.readCopyStorage(ctx, token, 2, false)
	if err != nil {
		return pair, err
	}
	if uniqueOpenBaoObject(storage.pods, r.Plan.Input.Member) != nil || uniqueOpenBaoObject(storage.pods, r.CopyPod.Name) != nil {
		return pair, errOpenBaoProviderStorage
	}
	if err := verifyOpenBaoDestinationsDetached(ctx, j.client, r.Destinations); err != nil {
		return pair, err
	}
	for index, intent := range r.Destinations {
		original := uniqueOpenBaoObject(storage.pvs, intent.Source.PV.Name)
		dest := uniqueOpenBaoObject(storage.pvs, intent.DestinationPV.Name)
		claim := uniqueOpenBaoObject(storage.claims, intent.Source.PVC.Name)
		if !openBaoRetainedReleasedPV(original, intent.Source.PV, intent.Source.PVC, "hcloud", hetznerCSIDriver,
			intent.Source.ProviderHandle, intent.Source.Bytes) || !openBaoProviderVolumeDetached(storage.provider[:], intent.Source) ||
			uniqueOpenBaoObject(storage.claims, intent.StagingClaim.Name) != nil ||
			(claim != nil && !matchOpenBaoSuccessorClaim(claim, intent.replacementClaim())) {
			return pair, errOpenBaoProviderStorage
		}
		for _, ticket := range storage.tickets {
			if text(ticket, "spec", "source", "persistentVolumeName") == intent.Source.PV.Name {
				return pair, errOpenBaoProviderStorage
			}
		}
		backend, err := j.client.get(ctx, "volumes.longhorn.io", "longhorn-system", intent.DestinationVolume.Name, false)
		if err != nil || !intent.detachedV2Destination(backend) || benchmarkIdentity(dest) != intent.DestinationPV {
			return pair, errOpenBaoProviderStorage
		}
		pair[index] = openBaoReboundStorage{Intent: intent, Original: original, Dest: dest, Backend: backend, NewClaim: claim}
	}
	return pair, nil
}
