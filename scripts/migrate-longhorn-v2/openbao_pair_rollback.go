package main

import (
	"context"
	"time"
)

// Roll back only while both original claims are still intact. Once the
// release intent is stored, retain all four PVs and finish the guarded rebind.
func (j *openBaoPairJournal) resumeOriginalPair(ctx context.Context, token, release string) error {
	r := j.record
	if j.client.context != "admin@prod" || !r.valid() || r.RolledBack ||
		(r.Phase != openBaoPairPlanned && r.Phase != openBaoPairStaged && r.Phase != openBaoPairCopied) ||
		r.StagePending != "" || r.FencePending != "" || r.CopyPending != "" {
		return errOpenBaoProviderStorage
	}
	plan := r.Plan
	for _, candidate := range []struct {
		id      benchmarkResource
		desired object
	}{
		{r.StageAssets[2], openBaoStagePod(plan.Run, plan.Source, plan.Original.Node)},
		{r.CopyAssets[2], openBaoCopyPod(plan.Run, plan.Source, plan.Input.Member, plan.Original.Node)},
	} {
		if candidate.id.UID == "" {
			continue
		}
		pod, err := j.client.get(ctx, "pod", openBaoNamespace, candidate.id.Name, true)
		if err != nil {
			return err
		}
		if pod != nil {
			if benchmarkIdentity(pod) != candidate.id || !j.client.matchBoundOpenBaoVolumePod(ctx, pod, pod, candidate.desired, plan.Original.Node) {
				return errOpenBaoProviderStorage
			}
			if err := removeScratch(ctx, j.client, plan.Source, "pod", openBaoNamespace,
				"/api/v1/namespaces/"+openBaoNamespace+"/pods/"+candidate.id.Name, pod); err != nil {
				return err
			}
		}
	}
	original, err := j.client.get(ctx, "pod", openBaoNamespace, plan.Input.Member, true)
	if err != nil {
		return err
	}
	if original == nil || value(original, "metadata", "deletionTimestamp") != nil {
		if !r.Evicting {
			return errOpenBaoProviderStorage
		}
		if err := j.waitForOfflinePair(ctx, token); err != nil {
			return err
		}
	} else if _, err := j.readServingOriginalPair(ctx, token); err != nil {
		return err
	}
	for _, index := range [2]int{1, 0} {
		if r.FenceAssets[index*2].UID != "" {
			if err := j.removeAdmissionFence(ctx, index); err != nil {
				return err
			}
		}
	}
	var restored benchmarkResource
	if err := waitReadyWithin(ctx, 6*time.Minute, func(ctx context.Context) error {
		var err error
		restored, err = j.readServingOriginalPair(ctx, token)
		return err
	}); err != nil {
		return err
	}
	if err := verifyOpenBaoRaftMember(ctx, j.client, restored, release, false); err != nil {
		return err
	}
	r.RolledBack = true
	return j.advance(ctx, r)
}

func (j openBaoPairJournal) readServingOriginalPair(ctx context.Context, token string) (benchmarkResource, error) {
	var unverified benchmarkResource
	plan := j.record.Plan
	storage, err := j.readCopyStorage(ctx, token, 3, true)
	if err != nil {
		return unverified, err
	}
	current, err := bindOpenBaoProviderStorage(plan.Input.Member, storage.stateful, storage.pods,
		storage.claims, storage.pvs, storage.tickets)
	server, ok := openBaoServerID(storage.node)
	if err != nil || !ok || current.StatefulSet != plan.Original.StatefulSet || current.Node != plan.Original.Node {
		return unverified, errOpenBaoProviderStorage
	}
	for index, original := range plan.Original.Disks {
		disk := current.Disks[index]
		if disk.PVC != original.PVC || disk.PV != original.PV || disk.ProviderHandle != original.ProviderHandle ||
			disk.Bytes != original.Bytes || !openBaoProviderAssignedToServer(storage.provider[:], disk, server) {
			return unverified, errOpenBaoProviderStorage
		}
	}
	return current.Member, nil
}
