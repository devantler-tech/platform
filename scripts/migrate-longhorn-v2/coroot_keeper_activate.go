package main

import (
	"context"
	"errors"
	"strconv"
	"time"
)

func (j *corootKeeperJournal) rebindAndResume(ctx context.Context, fences corootKeeperFences, disks []cnpgRecoveryDiskBinding) error {
	if j.record.Phase == "copied-volumes-detached" || j.record.Phase == "claim-release-requested" {
		if err := j.releaseClaims(ctx, fences); err != nil {
			return err
		}
	}
	if j.record.Phase == "original-and-staging-retained" {
		if err := j.requireKeeperFences(ctx, fences); err != nil {
			return err
		}
		if err := j.createDestinationClaim(ctx); err != nil {
			return err
		}
	}
	if j.record.Phase == "replacement-created" || j.record.Phase == "destination-rebind-requested" {
		if err := j.requireKeeperFences(ctx, fences); err != nil {
			return err
		}
		if err := j.bindDestination(ctx); err != nil {
			return err
		}
	}
	if j.record.Phase == "destination-bound" {
		if err := j.releaseKeeperPodFence(ctx, fences); err != nil {
			return err
		}
	}
	r := j.record
	if r.Phase != "member-released" {
		return errCorootKeeperReceipt
	}
	if err := j.requeueMissingKeeperMember(ctx, r, fences.pod); err != nil {
		return err
	}
	if err := waitReadyWithin(ctx, 5*time.Minute, func(ctx context.Context) error {
		return j.verifyRejoined(ctx, r, disks, j.source())
	}); err != nil {
		return err
	}
	if err := removeCorootKeeperFence(ctx, j.c, j.source(), fences.claim, r.ClaimFence); err != nil {
		return err
	}
	if err := j.verifyNewClaimBound(ctx, r); err != nil {
		return err
	}
	r.Phase = "replacement-verified"
	return j.advance(ctx, r)
}

// Removing an admission fence does not notify the StatefulSet controller. A
// metadata update requeues the same controller without changing its template
// or waiting through failed-create backoff. The claim UID makes the update
// idempotent when a verified rebind resumes.
func (j *corootKeeperJournal) requeueMissingKeeperMember(ctx context.Context, r corootKeeperCutoverRecord, podFence object) error {
	const annotation = "storage.platform.devantler.tech/keeper-rebind-claim"
	member := corootKeeperName + "-" + strconv.Itoa(r.Input.Ordinal)
	if r.NewClaim.Name != "data-"+member {
		return errCorootKeeperReceipt
	}
	pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, member, true)
	if err != nil || pod != nil {
		return err
	}
	for _, kind := range []string{keeperBindingKind, keeperPolicyKind} {
		fence, err := j.c.get(ctx, kind, "", text(podFence, "metadata", "name"), true)
		if err != nil || fence != nil {
			return errCorootKeeperReceipt
		}
	}
	if err := j.verifyNewClaimBound(ctx, r); err != nil {
		return err
	}
	set, err := j.c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || !recoveredNativeObject(set, "apps/v1", "StatefulSet", corootKeeperNamespace) ||
		benchmarkIdentity(set) != r.Before.Controller || value(set, "spec", "replicas") != float64(3) {
		return errCorootKeeperQuorum
	}
	prior := text(set, "metadata", "annotations", annotation)
	if prior == r.NewClaim.UID {
		return nil
	}
	if prior != "" && !isolationUID.MatchString(prior) {
		return errCorootKeeperReceipt
	}
	change := object{"op": "add", "path": "/metadata/annotations/storage.platform.devantler.tech~1keeper-rebind-claim", "value": r.NewClaim.UID}
	if value(set, "metadata", "annotations") == nil {
		change["path"], change["value"] = "/metadata/annotations", object{annotation: r.NewClaim.UID}
	}
	if err := sourceCurrent(ctx, j.c.run, j.source()); err != nil {
		return err
	}
	return j.c.patch(ctx, "statefulset", corootKeeperNamespace, set, change)
}

func (j *corootKeeperJournal) requireKeeperFences(ctx context.Context, fences corootKeeperFences) error {
	for _, fence := range []struct {
		desired object
		receipt benchmarkFenceReceipt
	}{{fences.pod, j.record.PodFence}, {fences.claim, j.record.ClaimFence}} {
		name := text(fence.desired, "metadata", "name")
		owner := text(fence.desired, "metadata", "annotations", corootKeeperFenceOwner)
		policy, err := j.c.get(ctx, keeperPolicyKind, "", name, false)
		if err != nil {
			return err
		}
		binding, err := j.c.get(ctx, keeperBindingKind, "", name, false)
		if err != nil || !matchesOwnedBenchmarkFence(policy, binding, fence.desired, fence.receipt) ||
			text(policy, "metadata", "annotations", corootKeeperFenceOwner) != owner ||
			text(binding, "metadata", "annotations", corootKeeperFenceOwner) != owner {
			return errCorootKeeperReceipt
		}
	}
	return nil
}

func (j *corootKeeperJournal) createDestinationClaim(ctx context.Context) error {
	r := j.record
	owner, original, staged, backend, err := j.readRebindAssets(ctx, r.Rebind)
	if err != nil {
		return err
	}
	desired, err := r.Rebind.replacementClaim(owner, original, staged, backend)
	if err != nil {
		return err
	}
	claimOwner := r.Run + ":" + r.Source
	valueObject(desired, "metadata")["annotations"] = object{corootKeeperFenceOwner: claimOwner}
	replacement, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.Rebind.Original.Claim.Name, true)
	if err != nil {
		return err
	}
	if replacement == nil {
		replacement, err = j.c.create(ctx, j.source(), desired)
		if err != nil {
			return err
		}
	}
	if !recoveredNativeObject(replacement, "v1", "PersistentVolumeClaim", corootKeeperNamespace) ||
		text(replacement, "metadata", "name") != r.Rebind.Original.Claim.Name ||
		text(replacement, "metadata", "annotations", corootKeeperFenceOwner) != claimOwner ||
		text(replacement, "metadata", "uid") == r.Rebind.Original.Claim.UID {
		return errCorootKeeperRebind
	}
	r.NewClaim, r.Phase = benchmarkIdentity(replacement), "replacement-created"
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) bindDestination(ctx context.Context) error {
	r := j.record
	if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
		return err
	}
	owner, original, staged, backend, err := j.readRebindAssets(ctx, r.Rebind)
	if err != nil {
		return err
	}
	if err := verifyCorootKeeperDetach(ctx, j.c, r.Rebind, true); err != nil {
		return err
	}
	if !keeperReleasedPV(original, r.Rebind.Original.PV, r.Rebind.Original.Claim, "hcloud", hetznerCSIDriver, r.Rebind.Original.Handle) ||
		benchmarkIdentity(staged) != r.Rebind.StagedPV ||
		text(staged, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		return errCorootKeeperRebind
	}
	if r.Phase == "replacement-created" {
		r.Phase = "destination-rebind-requested"
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	if text(staged, "spec", "claimRef", "uid") != r.NewClaim.UID {
		if err := j.patchDestinationClaimRef(ctx, r, owner, original, staged, backend); err != nil {
			return err
		}
	}
	if err := waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
		return j.verifyNewClaimBound(ctx, r)
	}); err != nil {
		return err
	}
	r.Phase = "destination-bound"
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) patchDestinationClaimRef(ctx context.Context, r corootKeeperCutoverRecord, owner, original, staged, backend object) error {
	var replacement object
	if err := waitReadyWithin(ctx, 30*time.Second, func(ctx context.Context) error {
		current, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.NewClaim.Name, false)
		if err != nil || benchmarkIdentity(current) != r.NewClaim ||
			text(current, "metadata", "annotations", corootKeeperFenceOwner) != r.Run+":"+r.Source {
			return errors.Join(errReadinessTerminal, errCorootKeeperRebind)
		}
		if text(current, "status", "phase") != "Pending" {
			return errCorootKeeperRebind
		}
		replacement = current
		return nil
	}); err != nil {
		return err
	}
	patch, err := r.Rebind.rebindChanges(owner, original, staged, backend, replacement)
	if err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.c.run, j.source()); err != nil {
		return err
	}
	return j.c.patch(ctx, "persistentvolume", "", staged, patch...)
}

func (j *corootKeeperJournal) releaseKeeperPodFence(ctx context.Context, fences corootKeeperFences) error {
	r := j.record
	if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
		// A previous attempt may have removed the fence and allowed the
		// replacement member to return before recording member-released.
		// A policy without a binding cannot prevent the replacement from joining.
		// The subsequent cleanup still requires the exact owned policy UID.
		binding, readErr := j.c.get(ctx, keeperBindingKind, "", text(fences.pod, "metadata", "name"), true)
		if readErr != nil || binding != nil {
			return err
		}
	}
	if err := j.verifyNewClaimBound(ctx, r); err != nil {
		return err
	}
	if err := removeCorootKeeperFence(ctx, j.c, j.source(), fences.pod, r.PodFence); err != nil {
		return err
	}
	r.Phase = "member-released"
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
	if r.Phase == "copied-volumes-detached" {
		r.Phase = "claim-release-requested"
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	for _, row := range []struct {
		claim, pv             benchmarkResource
		class, driver, handle string
	}{
		{r.Rebind.StagedClaim, r.Rebind.StagedPV, "longhorn-v2", "driver.longhorn.io", r.Rebind.StagedVolume.Name},
		{r.Rebind.Original.Claim, r.Rebind.Original.PV, "hcloud", hetznerCSIDriver, r.Rebind.Original.Handle},
	} {
		pv, err := j.c.get(ctx, "persistentvolume", "", row.pv.Name, false)
		if err != nil || !keeperRetainedPVBinding(pv, row.pv, row.claim, row.class, row.driver, row.handle) {
			return errCorootKeeperRebind
		}
		claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, row.claim.Name, true)
		if err != nil {
			return errCorootKeeperRebind
		}
		if err := j.requireNoClaimConsumers(ctx, row.claim.Name); err != nil {
			return err
		}
		if claim != nil {
			if benchmarkIdentity(claim) != row.claim || text(claim, "spec", "volumeName") != row.pv.Name ||
				text(claim, "spec", "storageClassName") != row.class || text(pv, "status", "phase") != "Bound" {
				return errCorootKeeperRebind
			}
			if value(claim, "metadata", "deletionTimestamp") == nil {
				path := "/api/v1/namespaces/" + corootKeeperNamespace + "/persistentvolumeclaims/" + row.claim.Name
				if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
					return err
				}
				if err := j.c.deleteObserved(ctx, j.source(), path, claim); err != nil {
					return err
				}
			}
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
	if err != nil || !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", corootKeeperNamespace) ||
		benchmarkIdentity(claim) != r.NewClaim || text(claim, "status", "phase") != "Bound" ||
		text(claim, "metadata", "annotations", corootKeeperFenceOwner) != r.Run+":"+r.Source ||
		text(claim, "spec", "storageClassName") != "longhorn-v2" || text(claim, "spec", "volumeName") != r.Rebind.StagedPV.Name ||
		!providerRWOFilesystem(claim) || text(claim, "spec", "resources", "requests", "storage") != corootKeeperDataSize ||
		value(claim, "spec", "dataSource") != nil || value(claim, "spec", "dataSourceRef") != nil {
		return errCorootKeeperRebind
	}
	pv, err := j.c.get(ctx, "persistentvolume", "", r.Rebind.StagedPV.Name, false)
	if err != nil || text(pv, "status", "phase") != "Bound" ||
		!keeperRetainedPVBinding(pv, r.Rebind.StagedPV, r.NewClaim, "longhorn-v2", "driver.longhorn.io", r.Rebind.StagedVolume.Name) {
		return errCorootKeeperRebind
	}
	return nil
}

func removeCorootKeeperFence(ctx context.Context, c client, source string, desired object, owned benchmarkFenceReceipt) error {
	name := text(desired, "metadata", "name")
	owner := text(desired, "metadata", "annotations", corootKeeperFenceOwner)
	if name == "" || owner == "" || !isolationUID.MatchString(owned.PolicyUID) {
		return errCorootKeeperReceipt
	}
	policy, err := c.get(ctx, keeperPolicyKind, "", name, true)
	if err != nil {
		return err
	}
	binding, err := c.get(ctx, keeperBindingKind, "", name, true)
	if err != nil {
		return err
	}
	if policy == nil && binding == nil {
		return nil
	}
	if !matchesOwnedBenchmarkPolicy(policy, desired, owned.PolicyUID) ||
		text(policy, "metadata", "annotations", corootKeeperFenceOwner) != owner {
		return errCorootKeeperReceipt
	}
	if binding == nil {
		return removeScratch(ctx, c, source, keeperPolicyKind, "", keeperPolicyAPI+"validatingadmissionpolicies/"+name, policy)
	}
	if !matchesOwnedBenchmarkFence(policy, binding, desired, owned) ||
		text(binding, "metadata", "annotations", corootKeeperFenceOwner) != owner {
		return errCorootKeeperReceipt
	}
	if err := removeScratch(ctx, c, source, keeperBindingKind, "", keeperPolicyAPI+"validatingadmissionpolicybindings/"+name, binding); err != nil {
		return err
	}
	return removeScratch(ctx, c, source, keeperPolicyKind, "", keeperPolicyAPI+"validatingadmissionpolicies/"+name, policy)
}

func (j *corootKeeperJournal) verifyRejoined(ctx context.Context, r corootKeeperCutoverRecord, disks []cnpgRecoveryDiskBinding, source string) error {
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
	var probed [3]corootKeeperMember
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
		if err != nil {
			return errCorootKeeperQuorum
		}
		probed[ordinal] = member
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
		} else if member.UID != expected.UID || member.Node != expected.Node {
			return errCorootKeeperQuorum
		}
	}
	if leaders != 1 {
		return errCorootKeeperQuorum
	}
	if err := verifyKeeperProbedMembers(ctx, j.c, stateful, probed); err != nil {
		return err
	}
	return sourceCurrent(ctx, j.c.run, source)
}
