package main

import (
	"context"
	"time"
)

func (j *openBaoPairJournal) releaseCopiedClaims(ctx context.Context, token string) error {
	r := j.record
	if j.client.context != "admin@prod" || !r.valid() || (r.Phase != openBaoPairCopied && r.Phase != openBaoPairReleasing) {
		return errOpenBaoProviderStorage
	}
	if err := j.proveAdmissionFences(ctx); err != nil {
		return err
	}
	if r.Phase == openBaoPairCopied {
		if err := j.waitForOfflinePair(ctx, token); err != nil {
			return err
		}
		if _, err := j.readClaimsForRelease(ctx, token); err != nil {
			return err
		}
		r.Phase = openBaoPairReleasing
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	for index := range 4 {
		claims, err := j.readClaimsForRelease(ctx, token)
		if err != nil {
			return err
		}
		claim := claims[index]
		if claim == nil {
			continue
		}
		if _, err := j.read(ctx); err != nil {
			return err
		}
		path := "/api/v1/namespaces/" + openBaoNamespace + "/persistentvolumeclaims/" + text(claim, "metadata", "name")
		if err := j.client.deleteObserved(ctx, r.Plan.Source, path, claim); err != nil {
			return err
		}
		if err := waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
			current, err := j.readClaimsForRelease(ctx, token)
			if err != nil || current[index] != nil {
				return errOpenBaoProviderStorage
			}
			return nil
		}); err != nil {
			return err
		}
	}
	r.Phase = openBaoPairSourcesReleased
	return j.advance(ctx, r)
}

func (j openBaoPairJournal) readClaimsForRelease(ctx context.Context, token string) ([4]object, error) {
	var claims [4]object
	r := j.record
	storage, err := j.readCopyStorage(ctx, token, 2, false)
	if err != nil {
		return claims, err
	}
	if err := verifyOpenBaoDestinationsDetached(ctx, j.client, r.Destinations); err != nil {
		return claims, err
	}
	for i, intent := range r.Destinations {
		if !openBaoProviderVolumeDetached(storage.provider[:], intent.Source) {
			return claims, errOpenBaoProviderStorage
		}
		stage, err := storage.claimForRelease(intent.StagingClaim, intent.DestinationPV, "longhorn-v2", "driver.longhorn.io",
			intent.DestinationVolume.Name, intent.Source.Bytes)
		if err != nil {
			return claims, err
		}
		original, err := storage.claimForRelease(intent.Source.PVC, intent.Source.PV, "hcloud", hetznerCSIDriver,
			intent.Source.ProviderHandle, intent.Source.Bytes)
		if err != nil {
			return claims, err
		}
		claims[i], claims[i+2] = stage, original
	}
	if uniqueOpenBaoObject(storage.pods, r.Plan.Input.Member) != nil || uniqueOpenBaoObject(storage.pods, r.CopyPod.Name) != nil {
		return claims, errOpenBaoProviderStorage
	}
	return claims, nil
}

func (s openBaoCopyStorage) claimForRelease(claimID, pvID benchmarkResource, class, driver, handle string, bytes uint64) (object, error) {
	claim, pv := uniqueOpenBaoObject(s.claims, claimID.Name), uniqueOpenBaoObject(s.pvs, pvID.Name)
	for _, mount := range s.tickets {
		if text(mount, "spec", "source", "persistentVolumeName") == pvID.Name {
			return nil, errOpenBaoProviderStorage
		}
	}
	for _, pod := range s.pods {
		volumes, _ := value(pod, "spec", "volumes").([]any)
		for _, volume := range volumes {
			entry, _ := volume.(object)
			if text(entry, "persistentVolumeClaim", "claimName") == claimID.Name {
				return nil, errOpenBaoProviderStorage
			}
		}
	}
	if claim == nil && openBaoRetainedReleasedPV(pv, pvID, claimID, class, driver, handle, bytes) {
		return nil, nil
	}
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", openBaoNamespace) || benchmarkIdentity(claim) != claimID ||
		!providerRWOFilesystem(claim) || text(claim, "status", "phase") != "Bound" || text(claim, "spec", "volumeName") != pvID.Name ||
		text(claim, "spec", "storageClassName") != class || providerVolumeBytes(text(claim, "status", "capacity", "storage")) != bytes ||
		text(pv, "status", "phase") != "Bound" || !openBaoRetainedPVBinding(pv, pvID, claimID, class, driver, handle, bytes) {
		return nil, errOpenBaoProviderStorage
	}
	return claim, nil
}
