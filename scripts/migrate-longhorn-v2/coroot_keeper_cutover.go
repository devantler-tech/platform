package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

func runCorootKeeperCutover(ctx context.Context, c client, source, run string, input corootKeeperCutoverInput) (result error) {
	if c.context != "admin@prod" || !validBenchmarkOwner(run, source) ||
		input.Ordinal < 0 || input.Ordinal > 2 || !retentionDigest.MatchString(input.Digest) {
		return errCorootKeeperReceipt
	}
	ctx, cancel := context.WithTimeout(ctx, 36*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err := verifyLonghornV2RetainClass(ctx, c); err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	prior, err := c.get(ctx, "configmap", "longhorn-system", corootKeeperJournalName(input.Ordinal), true)
	if err != nil {
		return err
	}
	if prior != nil {
		storage, err := activation.observeRecoveryStorage(ctx, nil)
		if err != nil {
			return err
		}
		rollback, err := bindCorootKeeperRollback(c, prior, input)
		if err != nil {
			return resumeCorootKeeperRebind(ctx, c, source, run, input, prior, storage.Disks)
		}
		if err := rollbackInterruptedCorootKeeper(ctx, rollback, source, run, storage.Disks); err != nil {
			return err
		}
	}
	capacity, err := activation.recoveryClaimStorage(ctx, corootKeeperDataSize)
	if err != nil {
		return err
	}
	before, err := readCorootKeeperQuorum(ctx, c, source, capacity.Disks)
	if err != nil {
		return err
	}
	candidate := before.Members[input.Ordinal]
	original := before.Disks[input.Ordinal]
	if candidate.Leader || original.Digest != input.Digest || before.V2Claims[input.Ordinal].UID != "" {
		return errCorootKeeperStorage
	}
	if err := verifyKeeperCopyHostname(ctx, c, candidate.Node); err != nil {
		return err
	}
	owner, err := c.get(ctx, "coroots.coroot.com", corootKeeperNamespace, "coroot", false)
	if err != nil || !recoveredNativeObject(owner, "coroot.com/v1", "Coroot", corootKeeperNamespace) {
		return errCorootKeeperReceipt
	}
	controller, err := c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || benchmarkIdentity(controller) != before.Controller {
		return errCorootKeeperReceipt
	}
	refs, _ := value(controller, "metadata", "ownerReferences").([]any)
	if len(refs) != 1 {
		return errCorootKeeperReceipt
	}
	controllerOwner, _ := refs[0].(object)
	if text(controllerOwner, "apiVersion") != "coroot.com/v1" || text(controllerOwner, "kind") != "Coroot" ||
		text(controllerOwner, "name") != "coroot" || text(controllerOwner, "uid") != text(owner, "metadata", "uid") ||
		value(controllerOwner, "controller") != true {
		return errCorootKeeperReceipt
	}
	fenceUser, err := corootKeeperFenceUser(ctx, c)
	if err != nil {
		return err
	}
	record := corootKeeperCutoverRecord{Version: 1, Run: run, Source: source, Phase: "planned", Input: input,
		Before: before, Coroot: benchmarkIdentity(owner), FenceUser: fenceUser}
	journal, err := beginCorootKeeperJournal(ctx, c, record)
	if err != nil {
		return err
	}
	defer func() {
		if result == nil {
			return
		}
		failedPhase := journal.record.Phase
		if mayRestoreOriginalKeeper(failedPhase) {
			restoreCtx, stop := context.WithTimeout(context.Background(), 4*time.Minute)
			defer stop()
			result = errors.Join(result, journal.restoreOriginalBeforeClaimRelease(restoreCtx, capacity.Disks, false))
		}
		result = fmt.Errorf("keeper migration held in phase %s: %w", failedPhase, result)
	}()
	if err := journal.prepareVolume(ctx, capacity.Disks); err != nil {
		return err
	}
	fences, err := journal.installFences(ctx)
	if err != nil {
		return err
	}
	if err := journal.evictFollower(ctx, fences, capacity.Disks); err != nil {
		return err
	}
	if err := journal.copyVolume(ctx, capacity.Disks); err != nil {
		return err
	}
	return journal.rebindAndResume(ctx, fences, capacity.Disks)
}

func mayRestoreOriginalKeeper(phase string) bool {
	switch phase {
	case "planned", "staging-claim-created", "staging-pod-created", "staging-volume-verified",
		"member-fenced-policy", "member-fenced", "claim-fenced-policy", "claim-fenced", "eviction-requested",
		"copy-isolation-created", "copy-gate-created", "copy-pod-created", "copy-permitted",
		"copy-bytes-verified", "copied-volumes-detached", keeperOriginalResuming:
		return true
	default:
		return false
	}
}

func (j *corootKeeperJournal) restoreOriginalBeforeClaimRelease(ctx context.Context, disks []cnpgRecoveryDiskBinding, priorRunFinished bool) error {
	if _, err := j.read(ctx); err != nil {
		return err
	}
	if err := j.reconcileKeeperCreation(ctx, priorRunFinished); err != nil {
		return err
	}
	r := j.record
	original := r.Before.Disks[r.Input.Ordinal]
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, original.Claim.Name, false)
	if err != nil || benchmarkIdentity(claim) != original.Claim || text(claim, "status", "phase") != "Bound" ||
		text(claim, "spec", "volumeName") != original.PV.Name || text(claim, "spec", "storageClassName") != "hcloud" {
		return errCorootKeeperRebind
	}
	pv, err := j.c.get(ctx, "persistentvolume", "", original.PV.Name, false)
	if err != nil || benchmarkIdentity(pv) != original.PV || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		return errCorootKeeperRebind
	}
	binding, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	if err != nil || len(binding) != 1 || providerRetentionDigest(binding[0]) != original.Digest {
		return errCorootKeeperRebind
	}
	podPath := "/api/v1/namespaces/" + corootKeeperNamespace + "/pods/"
	for _, pod := range []benchmarkResource{r.StagePod, r.CopyPod} {
		if err := j.removeRollbackAsset(ctx, pod, "pod", podPath); err != nil {
			return err
		}
	}
	if r.CopyPod.UID != "" {
		// A resumed rollback may find the original member already serving.
		podFence := keeperFencesFor(r).pod
		binding, err := j.c.get(ctx, keeperBindingKind, "", text(podFence, "metadata", "name"), true)
		if err != nil || (binding != nil && text(binding, "metadata", "uid") != r.PodFence.BindingUID) {
			return errCorootKeeperReceipt
		}
		if binding != nil {
			if err := j.requireNoClaimConsumers(ctx, original.Claim.Name); err != nil {
				return err
			}
		}
		if err := waitReadyWithin(ctx, 75*time.Second, func(ctx context.Context) error {
			return verifyCorootKeeperDetach(ctx, j.c, r.Rebind, binding != nil)
		}); err != nil {
			return err
		}
	}
	staging, err := j.observeRetainedKeeperStaging(ctx, r)
	if err != nil {
		return err
	}
	if r.RollbackStaging != (corootKeeperRetainedStage{}) && r.RollbackStaging != staging {
		return errCorootKeeperStaging
	}
	for _, asset := range []struct {
		id           benchmarkResource
		kind, prefix string
	}{
		{r.CopyGate, "configmap", "/api/v1/namespaces/" + corootKeeperNamespace + "/configmaps/"},
		{r.CopyIsolation, "ciliumnetworkpolicy.cilium.io", "/apis/cilium.io/v2/namespaces/" + corootKeeperNamespace + "/ciliumnetworkpolicies/"},
	} {
		if err := j.removeRollbackAsset(ctx, asset.id, asset.kind, asset.prefix); err != nil {
			return err
		}
	}
	r.RollbackStaging = staging
	if r.Phase != keeperOriginalResuming {
		// Commit to restoring the original claim before either fence can disappear.
		r.Phase = keeperOriginalResuming
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	fences := keeperFencesFor(r)
	for _, fence := range []struct {
		desired object
		receipt benchmarkFenceReceipt
	}{{fences.claim, r.ClaimFence}, {fences.pod, r.PodFence}} {
		if fence.receipt.PolicyUID == "" {
			continue
		}
		if err := removeCorootKeeperFence(ctx, j.c, j.source(), fence.desired, fence.receipt); err != nil {
			return err
		}
	}
	if err := j.verifyRolledBackFencesAbsent(ctx); err != nil {
		return err
	}
	if err := waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
		ready, err := readCorootKeeperQuorum(ctx, j.c, j.source(), disks)
		if err != nil || ready.Disks[r.Input.Ordinal].Digest != original.Digest || ready.Controller != r.Before.Controller ||
			ready.Budget != r.Before.Budget {
			return errCorootKeeperQuorum
		}
		return nil
	}); err != nil {
		return err
	}
	r.Phase = keeperOriginalPreserved
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) removeRollbackAsset(ctx context.Context, id benchmarkResource, kind, prefix string) error {
	if id.UID == "" {
		return nil
	}
	current, err := j.c.get(ctx, kind, corootKeeperNamespace, id.Name, true)
	if err != nil || current == nil {
		return err
	}
	if benchmarkIdentity(current) != id ||
		text(current, "metadata", "annotations", corootKeeperFenceOwner) != j.record.Run+":"+j.record.Source {
		return errCorootKeeperReceipt
	}
	return removeScratch(ctx, j.c, j.source(), kind, corootKeeperNamespace, prefix+id.Name, current)
}

func verifyLonghornV2RetainClass(ctx context.Context, c client) error {
	sc, err := c.get(ctx, "storageclass", "", "longhorn-v2", false)
	parameters := object{"backupTargetName": "default", "dataEngine": "v2", "dataLocality": "disabled",
		"fsType": "ext4", "numberOfReplicas": "3", "staleReplicaTimeout": "30", "unmapMarkSnapChainRemoved": "disabled"}
	if err != nil || !recoveredNativeObject(sc, "storage.k8s.io/v1", "StorageClass", "") ||
		text(sc, "metadata", "name") != "longhorn-v2" || text(sc, "provisioner") != "driver.longhorn.io" ||
		text(sc, "reclaimPolicy") != "Retain" || text(sc, "volumeBindingMode") != "WaitForFirstConsumer" ||
		!reflect.DeepEqual(value(sc, "parameters"), parameters) {
		return errCorootKeeperStaging
	}
	return nil
}
