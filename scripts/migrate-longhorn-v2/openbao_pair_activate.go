package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func (j *openBaoPairJournal) resumeWithV2Pair(ctx context.Context, token, release string, disks []cnpgRecoveryDiskBinding) error {
	r := j.record
	if j.client.context != "admin@prod" || !r.valid() ||
		(r.Phase != openBaoPairRebound && r.Phase != openBaoPairResuming) {
		return errOpenBaoProviderStorage
	}
	if r.Phase == openBaoPairRebound {
		if err := j.proveAdmissionFences(ctx); err != nil {
			return err
		}
		pair, err := j.readRebindPair(ctx, token)
		if err != nil || verifyOpenBaoReboundPair(r.Plan.Input.Member, pair, r.Successors) != nil {
			return errOpenBaoProviderStorage
		}
		r.Phase = openBaoPairResuming
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	if err := j.removeAdmissionFence(ctx, 0); err != nil {
		return err
	}
	var replacement benchmarkResource
	if err := waitReadyWithin(ctx, 6*time.Minute, func(ctx context.Context) error {
		var err error
		replacement, err = j.readServingV2Pair(ctx, token, disks)
		return err
	}); err != nil {
		return err
	}
	if err := verifyOpenBaoRaftMember(ctx, j.client, replacement, release, false); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.client.run, r.Plan.Source); err != nil {
		return err
	}
	const deliver = `unset HCLOUD_TOKEN; exec "${RUNNER_TEMP:?}/bin/replace-openbao-pod" deliver --timeout 5m`
	out, err := j.client.run(ctx, nil, "bash", "-ec", deliver)
	if err != nil || strings.TrimSpace(string(out)) != "OPENBAO_SECRET_DELIVERY=CONFIRMED" {
		return errOpenBaoProviderStorage
	}
	if err := j.removeAdmissionFence(ctx, 1); err != nil {
		return err
	}
	if err := j.removeCopyIsolation(ctx); err != nil {
		return err
	}
	current, err := j.readServingV2Pair(ctx, token, disks)
	if err != nil || current != replacement {
		return errOpenBaoProviderStorage
	}
	r.Phase, r.Replacement = openBaoPairReady, replacement
	return j.advance(ctx, r)
}

func (j openBaoPairJournal) readServingV2Pair(ctx context.Context, token string, disks []cnpgRecoveryDiskBinding) (benchmarkResource, error) {
	var unverified benchmarkResource
	r := j.record
	storage, err := j.readCopyStorage(ctx, token, 3, true)
	if err != nil {
		return unverified, err
	}
	pod := uniqueOpenBaoObject(storage.pods, r.Plan.Input.Member)
	identity := benchmarkIdentity(pod)
	if !openBaoOriginalStorageMember(pod, storage.stateful) || identity.UID == r.Plan.Original.Member.UID ||
		!openBaoExactStorageMount(pod, "data") || !openBaoExactStorageMount(pod, "audit") {
		return unverified, errOpenBaoProviderStorage
	}
	for _, peer := range r.Plan.Survivors {
		if text(pod, "spec", "nodeName") == peer.Node.Name {
			return unverified, errOpenBaoProviderStorage
		}
	}
	for index, intent := range r.Destinations {
		original := uniqueOpenBaoObject(storage.pvs, intent.Source.PV.Name)
		claim := uniqueOpenBaoObject(storage.claims, r.Successors[index].Name)
		if !openBaoRetainedReleasedPV(original, intent.Source.PV, intent.Source.PVC, "hcloud", hetznerCSIDriver,
			intent.Source.ProviderHandle, intent.Source.Bytes) ||
			!openBaoProviderVolumeDetached(storage.provider[:], intent.Source) ||
			benchmarkIdentity(claim) != r.Successors[index] || !matchOpenBaoSuccessorClaim(claim, intent.replacementClaim()) ||
			!openBaoCopyIsOnlyServingConsumer(claim, pod, storage.pods) {
			return unverified, errOpenBaoProviderStorage
		}
		bytes := strconv.FormatUint(intent.Source.Bytes, 10)
		size := strconv.FormatUint(intent.Source.Bytes>>30, 10) + "Gi"
		pv, volume, err := readQualifiedV2Attachment(ctx, j.client, claim, text(pod, "spec", "nodeName"),
			openBaoNamespace, size, bytes, disks, errOpenBaoProviderStorage)
		if err != nil || pv != intent.DestinationPV || volume != intent.DestinationVolume {
			return unverified, errOpenBaoProviderStorage
		}
	}
	return identity, nil
}

func openBaoCopyIsOnlyServingConsumer(claim, member object, pods []object) bool {
	name := text(claim, "metadata", "name")
	component, _, _ := strings.Cut(name, "-")
	matched := false
	for _, pod := range pods {
		volumes, _ := value(pod, "spec", "volumes").([]any)
		for _, raw := range volumes {
			volume, _ := raw.(object)
			if text(volume, "persistentVolumeClaim", "claimName") != name {
				continue
			}
			if matched || benchmarkIdentity(pod) != benchmarkIdentity(member) ||
				text(volume, "name") != component || value(volume, "persistentVolumeClaim", "readOnly") == true {
				return false
			}
			matched = true
		}
	}
	return matched
}
