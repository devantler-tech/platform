package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

func (j *openBaoPairJournal) stageDestinations(ctx context.Context, qualified []cnpgRecoveryDiskBinding) error {
	record := j.record
	if j.client.context != "admin@prod" || record.Phase != openBaoPairPlanned || record.StagePending != "" {
		return errOpenBaoProviderStorage
	}
	plan := record.Plan
	claims := openBaoStagingClaims(plan.Run, plan.Source)
	desired := [3]object{claims[0], claims[1], openBaoStagePod(plan.Run, plan.Source, plan.Original.Node)}
	kinds := [3]string{"persistentvolumeclaim", "persistentvolumeclaim", "pod"}
	var assets [3]object
	for i, asset := range desired {
		observed, err := j.readOrCreateStageAsset(ctx, i, kinds[i], asset)
		if err != nil {
			return err
		}
		assets[i] = observed
	}
	var destinations [2]openBaoStorageRebind
	if err := waitReadyWithin(ctx, 4*time.Minute, func(ctx context.Context) error {
		var err error
		destinations, err = readOpenBaoStagedPair(ctx, j.client, plan, assets[2], [2]object{assets[0], assets[1]}, qualified)
		return err
	}); err != nil {
		return err
	}
	next := j.record
	next.Phase, next.Destinations = openBaoPairStaged, destinations
	if err := j.advance(ctx, next); err != nil {
		return err
	}
	pod := next.StageAssets[2]
	if err := removeScratch(ctx, j.client, plan.Source, "pod", openBaoNamespace,
		"/api/v1/namespaces/"+openBaoNamespace+"/pods/"+pod.Name, assets[2]); err != nil {
		return err
	}
	return waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		return verifyOpenBaoDestinationsDetached(ctx, j.client, destinations)
	})
}

func (j *openBaoPairJournal) readOrCreateStageAsset(ctx context.Context, index int, kind string, desired object) (object, error) {
	plan := j.record.Plan
	name := text(desired, "metadata", "name")
	record := j.record
	before := record.StageAssets[index]
	if before != (benchmarkResource{}) {
		observed, err := j.client.get(ctx, kind, openBaoNamespace, name, false)
		if err != nil || benchmarkIdentity(observed) != before || !j.matchOpenBaoStageAsset(ctx, index, observed, desired) {
			return nil, errOpenBaoProviderStorage
		}
		return observed, nil
	}
	record.StagePending = openBaoStageAssetKinds[index]
	if err := j.advance(ctx, record); err != nil {
		return nil, err
	}
	created, err := j.client.create(ctx, plan.Source, desired)
	if err != nil || !j.matchOpenBaoStageAsset(ctx, index, created, desired) {
		return nil, errors.Join(err, errOpenBaoProviderStorage)
	}
	record.StagePending, record.StageAssets[index] = "", benchmarkIdentity(created)
	if err := j.advance(ctx, record); err != nil {
		return nil, err
	}
	return created, nil
}

func (j openBaoPairJournal) matchOpenBaoStageAsset(ctx context.Context, index int, observed, desired object) bool {
	if index == 2 {
		return j.client.matchBoundOpenBaoVolumePod(ctx, observed, observed, desired, j.record.Plan.Original.Node)
	}
	return matchOpenBaoStagedClaim(observed, desired)
}

func verifyOpenBaoDestinationsDetached(ctx context.Context, c client, destinations [2]openBaoStorageRebind) error {
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	for _, intent := range destinations {
		volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", intent.DestinationVolume.Name, false)
		if err != nil || !intent.detachedV2Destination(volume) {
			return errOpenBaoProviderStorage
		}
		native, err := c.get(ctx, "volumeattachments.longhorn.io", "longhorn-system", intent.DestinationVolume.Name, false)
		if err != nil || !recoveredNativeObject(native, "longhorn.io/v1beta2", "VolumeAttachment", "longhorn-system") ||
			!recoveredVolumeOwner(native, volume) || text(native, "spec", "volume") != intent.DestinationVolume.Name ||
			!absentOrEmptyMap(value(native, "spec", "attachmentTickets")) ||
			!absentOrEmptyMap(value(native, "status", "attachmentTicketStatuses")) {
			return errOpenBaoProviderStorage
		}
		for _, attachment := range attachments {
			if text(attachment, "spec", "source", "persistentVolumeName") == intent.DestinationPV.Name {
				return errOpenBaoProviderStorage
			}
		}
	}
	return nil
}

func readOpenBaoStagedPair(ctx context.Context, c client, plan openBaoPairPlan, stagedPod object,
	stagedClaims [2]object, qualified []cnpgRecoveryDiskBinding) ([2]openBaoStorageRebind, error) {
	var intents [2]openBaoStorageRebind
	if verifyOpenBaoPairPlan(plan, plan.Run, plan.Source) != nil {
		return intents, errOpenBaoProviderStorage
	}
	podName := text(stagedPod, "metadata", "name")
	pod, err := c.get(ctx, "pod", openBaoNamespace, podName, false)
	desiredPod := openBaoStagePod(plan.Run, plan.Source, plan.Original.Node)
	if err != nil || !c.matchBoundOpenBaoVolumePod(ctx, pod, stagedPod, desiredPod, plan.Original.Node) ||
		text(pod, "spec", "nodeName") != plan.Original.Node ||
		text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) {
		return intents, errOpenBaoProviderStorage
	}
	raw, err := c.kubectl(ctx, nil, "logs", podName, "--namespace="+openBaoNamespace, "--container=verify")
	if err != nil || !verifyOpenBaoStagedFilesystemReceipt(string(raw), plan.Original.Disks) {
		return intents, errOpenBaoProviderStorage
	}
	desired := openBaoStagingClaims(plan.Run, plan.Source)
	for i, disk := range plan.Original.Disks {
		identity := benchmarkIdentity(stagedClaims[i])
		claim, err := c.get(ctx, "persistentvolumeclaim", openBaoNamespace, identity.Name, false)
		if err != nil || benchmarkIdentity(claim) != identity || !matchOpenBaoStagedClaim(claim, desired[i]) ||
			text(claim, "spec", "volumeName") != "pvc-"+identity.UID {
			return [2]openBaoStorageRebind{}, errOpenBaoProviderStorage
		}
		size := text(desired[i], "spec", "resources", "requests", "storage")
		pv, volume, err := readQualifiedV2Attachment(ctx, c, claim, plan.Original.Node, openBaoNamespace,
			size, strconv.FormatUint(disk.Bytes, 10), qualified, errOpenBaoProviderStorage)
		if err != nil {
			return [2]openBaoStorageRebind{}, err
		}
		intents[i] = openBaoStorageRebind{Source: disk, StagingClaim: identity, DestinationPV: pv,
			DestinationVolume: volume, Run: plan.Run, SourceSHA: plan.Source}
	}
	if !openBaoRebindPairMatches(plan.Input.Member, intents) {
		return [2]openBaoStorageRebind{}, errOpenBaoProviderStorage
	}
	fresh, err := c.get(ctx, "pod", openBaoNamespace, podName, false)
	if err != nil || !c.matchBoundOpenBaoVolumePod(ctx, fresh, pod, desiredPod, plan.Original.Node) ||
		text(fresh, "spec", "nodeName") != plan.Original.Node ||
		text(fresh, "status", "phase") != "Running" || !ready(valueObject(fresh, "status")) {
		return [2]openBaoStorageRebind{}, errOpenBaoProviderStorage
	}
	return intents, nil
}

func matchOpenBaoStagedClaim(observed, desired object) bool {
	return recoveredNativeObject(observed, "v1", "PersistentVolumeClaim", openBaoNamespace) &&
		text(observed, "metadata", "name") == text(desired, "metadata", "name") &&
		text(observed, "metadata", "annotations", openBaoCopyFenceOwner) == text(desired, "metadata", "annotations", openBaoCopyFenceOwner) &&
		value(observed, "metadata", "ownerReferences") == nil && providerRWOFilesystem(observed) &&
		text(observed, "spec", "storageClassName") == "longhorn-v2" &&
		text(observed, "spec", "resources", "requests", "storage") == text(desired, "spec", "resources", "requests", "storage") &&
		value(observed, "spec", "dataSource") == nil && value(observed, "spec", "dataSourceRef") == nil &&
		value(observed, "spec", "selector") == nil
}

func verifyOpenBaoStagedFilesystemReceipt(log string, original [2]openBaoProviderDisk) bool {
	data, audit, found := strings.Cut(strings.TrimSuffix(log, "\n"), " audit=")
	if !found {
		return false
	}
	data, found = strings.CutPrefix(data, "OPENBAO_DESTINATIONS=READY data=")
	if !found {
		return false
	}
	for i, count := range [2]string{data, audit} {
		free, err := strconv.ParseUint(count, 10, 64)
		if err != nil || strconv.FormatUint(free, 10) != count || free == 0 || free > original[i].Bytes {
			return false
		}
	}
	return true
}
