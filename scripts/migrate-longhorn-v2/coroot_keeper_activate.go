package main

import (
	"context"
	"errors"
	"time"
)

func (j *corootKeeperJournal) rebindAndResume(ctx context.Context, fences corootKeeperFences, disks []cnpgRecoveryDiskBinding) error {
	r := j.record
	if r.Phase != "copied-volumes-detached" {
		return errCorootKeeperReceipt
	}
	if err := j.releaseClaims(ctx, fences); err != nil {
		return err
	}
	r = j.record
	owner, original, staged, backend, err := j.readRebindAssets(ctx, r.Rebind)
	if err != nil {
		return err
	}
	desired, err := r.Rebind.replacementClaim(owner, original, staged, backend)
	if err != nil {
		return err
	}
	replacement, err := j.c.create(ctx, r.Source, desired)
	if err != nil {
		return err
	}
	if !recoveredNativeObject(replacement, "v1", "PersistentVolumeClaim", corootKeeperNamespace) ||
		text(replacement, "metadata", "name") != r.Rebind.Original.Claim.Name ||
		text(replacement, "metadata", "uid") == r.Rebind.Original.Claim.UID {
		return errCorootKeeperRebind
	}
	r.NewClaim, r.Phase = benchmarkIdentity(replacement), "replacement-created"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	err = waitReadyWithin(ctx, 30*time.Second, func(ctx context.Context) error {
		current, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.NewClaim.Name, false)
		if err != nil || benchmarkIdentity(current) != r.NewClaim {
			return errors.Join(errReadinessTerminal, errCorootKeeperRebind)
		}
		if text(current, "status", "phase") != "Pending" {
			return errCorootKeeperRebind
		}
		replacement = current
		return nil
	})
	if err != nil {
		return err
	}
	patch, err := r.Rebind.rebindChanges(owner, original, staged, backend, replacement)
	if err != nil {
		return err
	}
	r.Phase = "destination-rebind-requested"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.c.run, r.Source); err != nil {
		return err
	}
	if err := j.c.patch(ctx, "persistentvolume", "", staged, patch...); err != nil {
		return err
	}
	if err := waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
		return j.verifyNewClaimBound(ctx, r)
	}); err != nil {
		return err
	}
	r.Phase = "destination-bound"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
		return err
	}
	if err := removeCorootKeeperFence(ctx, j.c, r.Source, fences.pod, r.PodFence); err != nil {
		return err
	}
	r.Phase = "member-released"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	if err := waitReadyWithin(ctx, 5*time.Minute, func(ctx context.Context) error {
		return j.verifyRejoined(ctx, r, disks)
	}); err != nil {
		return err
	}
	r.Phase = "replacement-verified"
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) releaseClaims(ctx context.Context, fences corootKeeperFences) error {
	r := j.record
	if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
		return err
	}
	if err := verifyCorootKeeperDetach(ctx, j.c, r.Rebind, true); err != nil {
		return err
	}
	if err := j.proveFences(ctx, fences); err != nil {
		return err
	}
	r.Phase = "claim-release-requested"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	for _, row := range []struct {
		claim, pv             benchmarkResource
		class, driver, handle string
	}{
		{r.Rebind.StagedClaim, r.Rebind.StagedPV, "longhorn-v2", "driver.longhorn.io", r.Rebind.StagedVolume.Name},
		{r.Rebind.Original.Claim, r.Rebind.Original.PV, "hcloud", hetznerCSIDriver, r.Rebind.Original.Handle},
	} {
		pv, err := j.c.get(ctx, "persistentvolume", "", row.pv.Name, false)
		if err != nil || benchmarkIdentity(pv) != row.pv ||
			text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
			text(pv, "spec", "claimRef", "uid") != row.claim.UID ||
			text(pv, "status", "phase") != "Bound" ||
			text(pv, "spec", "storageClassName") != row.class ||
			text(pv, "spec", "csi", "driver") != row.driver || text(pv, "spec", "csi", "volumeHandle") != row.handle {
			return errCorootKeeperRebind
		}
		claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, row.claim.Name, false)
		if err != nil || benchmarkIdentity(claim) != row.claim || text(claim, "spec", "volumeName") != row.pv.Name {
			return errCorootKeeperRebind
		}
		if err := j.requireNoClaimConsumers(ctx, row.claim.Name); err != nil {
			return err
		}
		if err := j.c.deleteObserved(ctx, r.Source, "/api/v1/namespaces/"+corootKeeperNamespace+"/persistentvolumeclaims/"+row.claim.Name, claim); err != nil {
			return err
		}
		err = waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
			current, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, row.claim.Name, true)
			if err != nil || current != nil {
				return errCorootKeeperRebind
			}
			pv, err := j.c.get(ctx, "persistentvolume", "", row.pv.Name, false)
			if err != nil || !keeperReleasedPV(pv, row.pv, row.claim, row.class, row.driver, row.handle) {
				return errCorootKeeperRebind
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	r.Phase = "original-and-staging-retained"
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) requireNoClaimConsumers(ctx context.Context, claim string) error {
	pods, err := j.c.list(ctx, "pods", corootKeeperNamespace)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		volumes, _ := value(pod, "spec", "volumes").([]any)
		for _, raw := range volumes {
			volume, _ := raw.(object)
			if text(volume, "persistentVolumeClaim", "claimName") == claim {
				return errCorootKeeperRebind
			}
		}
	}
	return nil
}

func (j *corootKeeperJournal) readRebindAssets(ctx context.Context, binding corootKeeperRebind) (object, object, object, object, error) {
	owner, err := j.c.get(ctx, "coroots.coroot.com", corootKeeperNamespace, binding.Coroot.Name, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	original, err := j.c.get(ctx, "persistentvolume", "", binding.Original.PV.Name, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	staged, err := j.c.get(ctx, "persistentvolume", "", binding.StagedPV.Name, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	backend, err := j.c.get(ctx, "volumes.longhorn.io", "longhorn-system", binding.StagedVolume.Name, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return owner, original, staged, backend, nil
}

func (j *corootKeeperJournal) verifyNewClaimBound(ctx context.Context, r corootKeeperCutoverRecord) error {
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.NewClaim.Name, false)
	if err != nil || benchmarkIdentity(claim) != r.NewClaim || text(claim, "status", "phase") != "Bound" ||
		text(claim, "spec", "storageClassName") != "longhorn-v2" || text(claim, "spec", "volumeName") != r.Rebind.StagedPV.Name {
		return errCorootKeeperRebind
	}
	pv, err := j.c.get(ctx, "persistentvolume", "", r.Rebind.StagedPV.Name, false)
	if err != nil || benchmarkIdentity(pv) != r.Rebind.StagedPV || text(pv, "status", "phase") != "Bound" ||
		text(pv, "spec", "claimRef", "uid") != r.NewClaim.UID || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != r.Rebind.StagedVolume.Name {
		return errCorootKeeperRebind
	}
	return nil
}

func removeCorootKeeperFence(ctx context.Context, c client, source string, desired object, owned benchmarkFenceReceipt) error {
	name := text(desired, "metadata", "name")
	owner := text(desired, "metadata", "annotations", corootKeeperFenceOwner)
	policy, err := c.get(ctx, keeperPolicyKind, "", name, false)
	if err != nil || !matchesOwnedBenchmarkPolicy(policy, desired, owned.PolicyUID) ||
		text(policy, "metadata", "annotations", corootKeeperFenceOwner) != owner {
		return errCorootKeeperReceipt
	}
	if owned.BindingUID == "" {
		binding, err := c.get(ctx, keeperBindingKind, "", name, true)
		if err != nil || binding != nil {
			return errCorootKeeperReceipt
		}
		return removeScratch(ctx, c, source, keeperPolicyKind, "", keeperPolicyAPI+"validatingadmissionpolicies/"+name, policy)
	}
	binding, err := c.get(ctx, keeperBindingKind, "", name, false)
	if err != nil || !matchesOwnedBenchmarkFence(policy, binding, desired, owned) ||
		text(binding, "metadata", "annotations", corootKeeperFenceOwner) != owner {
		return errCorootKeeperReceipt
	}
	if err := removeScratch(ctx, c, source, keeperBindingKind, "", keeperPolicyAPI+"validatingadmissionpolicybindings/"+name, binding); err != nil {
		return err
	}
	return removeScratch(ctx, c, source, keeperPolicyKind, "", keeperPolicyAPI+"validatingadmissionpolicies/"+name, policy)
}

func (j *corootKeeperJournal) verifyRejoined(ctx context.Context, r corootKeeperCutoverRecord, disks []cnpgRecoveryDiskBinding) error {
	stateful, err := j.c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || benchmarkIdentity(stateful) != r.Before.Controller {
		return errCorootKeeperQuorum
	}
	budget, err := j.c.get(ctx, "poddisruptionbudget", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || benchmarkIdentity(budget) != r.Before.Budget || !corootKeeperControllerReady(stateful, budget) {
		return errCorootKeeperQuorum
	}
	original, err := j.c.get(ctx, "persistentvolume", "", r.Rebind.Original.PV.Name, false)
	if err != nil || !keeperReleasedPV(original, r.Rebind.Original.PV, r.Rebind.Original.Claim, "hcloud", hetznerCSIDriver, r.Rebind.Original.Handle) {
		return errCorootKeeperRebind
	}
	attachments, err := j.c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == r.Rebind.Original.PV.Name {
			return errCorootKeeperRebind
		}
	}
	if err := j.verifyNewClaimBound(ctx, r); err != nil {
		return err
	}
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.NewClaim.Name, false)
	if err != nil || benchmarkIdentity(claim) != r.NewClaim {
		return errCorootKeeperRebind
	}
	leaders := 0
	for ordinal, expected := range r.Before.Members {
		pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, expected.Pod, false)
		if err != nil {
			return err
		}
		member, err := bindCorootKeeperMember(pod, stateful, ordinal)
		if err != nil {
			return err
		}
		leader, err := probeCorootKeeperMember(ctx, j.c, member, stateful, ordinal, 2)
		if err != nil || leader != expected.Leader {
			return errCorootKeeperQuorum
		}
		if leader {
			leaders++
		}
		if ordinal == r.Input.Ordinal {
			if member.UID == expected.UID || member.ContainerID == expected.ContainerID {
				return errCorootKeeperQuorum
			}
			pv, volume, err := readCorootKeeperV2Attachment(ctx, j.c, claim, member.Node, disks)
			if err != nil || pv != r.Rebind.StagedPV || volume != r.Rebind.StagedVolume {
				return errCorootKeeperRebind
			}
		} else if member.UID != expected.UID || member.ContainerID != expected.ContainerID || member.Node != expected.Node {
			return errCorootKeeperQuorum
		}
	}
	if leaders != 1 {
		return errCorootKeeperQuorum
	}
	return sourceCurrent(ctx, j.c.run, r.Source)
}
