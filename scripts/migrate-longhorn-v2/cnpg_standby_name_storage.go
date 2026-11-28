package main

import (
	"context"
	"errors"
	"time"
)

func observeUmamiUnchangedPeers(ctx context.Context, j *umamiStandbyReplacementJournal) (cnpgObservation, []object, error) {
	c, record := j.c, j.record
	namespace := record.Original.Database.Namespace
	cluster, err := c.get(ctx, "clusters.postgresql.cnpg.io", namespace, record.Original.Database.Name, false)
	_, admitted := cnpgStandbyTransition(cluster)
	if err != nil || !admitted || benchmarkIdentity(cluster) != record.Original.Database ||
		!cnpgCondition(cluster, "ContinuousArchiving") || verifyUmamiReplacementPrimary(ctx, c, cluster, record.Primary) != nil {
		return cnpgObservation{}, nil, errors.New("the witnessed Umami write leader and archive are unverified")
	}
	all, err := c.list(ctx, "pods", namespace)
	if err != nil {
		return cnpgObservation{}, nil, err
	}
	observation := cnpgObservation{Cluster: cluster, States: map[string]postgresState{}, umamiRecoveryPeers: record.OtherPods}
	for _, identity := range record.OtherPods {
		pod := cnpgRecoveryPodByName(all, identity.Name)
		if benchmarkIdentity(pod) != identity || verifyPostgresPod(pod, pod, cluster) != nil {
			return cnpgObservation{}, nil, errors.New("a protected Umami member changed or is not ready")
		}
		observation.Pods = append(observation.Pods, pod)
	}
	return observation, all, nil
}

func verifyUmamiNameReleaseArchive(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	c := j.c
	observation, _, err := observeUmamiUnchangedPeers(ctx, j)
	if err != nil {
		return err
	}
	if err = c.readCNPGPhysicalState(ctx, &observation); err != nil {
		return err
	}
	primary := observation.States[j.record.Primary.Name]
	for name, state := range observation.States {
		if name != j.record.Primary.Name {
			if err = verifyPostgresStandby(primary, state); err != nil {
				return err
			}
		}
	}
	database := cnpgSource{Namespace: j.record.Original.Database.Namespace, Name: j.record.Original.Database.Name}
	archive, err := rebindCNPGStandbyArchiveProof(ctx, c, database, observation)
	if err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: j.executor().Source, owner: j.executor().Run, pools: c.inventoryPools}
	qualified, err := activation.recoveryStorage(ctx, archive.Plan.Source)
	if err != nil || !recoveryJSONEqual(qualified, *archive.Storage) {
		return errors.New("the independently restored Umami V2 archive capacity is unverified")
	}
	spare, err := readCNPGV2Standby(ctx, c, observation, j.record.Original.Spare.Pod.Name, qualified.Disks)
	if err != nil || !recoveryJSONEqual(spare, j.record.Original.Spare) {
		return errors.New("the original Umami V2 spare and its three replicas are unverified")
	}
	return nil
}

func readUmamiNameReleaseState(ctx context.Context, j *umamiStandbyReplacementJournal, join benchmarkResource, requireJob bool) (object, object, error) {
	c, original := j.c, j.record.Original
	observation, pods, err := observeUmamiUnchangedPeers(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	job, err := c.get(ctx, "jobs.batch", join.Namespace, join.Name, !requireJob)
	if err != nil {
		return nil, nil, err
	}
	if requireJob && !completedUmamiReusedJoin(job, observation.Cluster, original, join) || !requireJob && job != nil {
		return nil, nil, errors.New("the original completed Umami join job state is unverified")
	}
	restore, err := c.get(ctx, "jobs.batch", join.Namespace, original.Consumer.Name+"-restore", true)
	if err != nil || restore != nil {
		return nil, nil, errors.New("the retained Umami ordinal has an unverified restore job")
	}
	for _, pod := range pods {
		if text(pod, "metadata", "name") == original.Consumer.Name {
			return nil, nil, errors.New("a serving pod still holds the original Umami ordinal")
		}
		if cnpgOwned(pod, observation.Cluster) && j.record.OtherPods[text(pod, "metadata", "name")] != benchmarkIdentity(pod) {
			return nil, nil, errors.New("the Umami serving roster changed while retaining its previous claim")
		}
		if !podUsesUmamiClaim(pod, original.Claim.Name) {
			continue
		}
		if !requireJob || !completedUmamiJoinPod(pod, join) {
			return nil, nil, errors.New("an unverified pod still references the original Umami claim")
		}
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", original.Claim.Namespace, original.Claim.Name, false)
	if err != nil {
		return nil, nil, err
	}
	pv, backend, err := readDetachedUmamiBackend(ctx, c, original, "Bound")
	if err != nil {
		return nil, nil, err
	}
	profile, admitted := cnpgStandbyTransition(observation.Cluster)
	retained := false
	if original.Provider != nil {
		retained = providerCNPGOriginalClaimMatches(*original.Provider, claim, pv)
	} else {
		retained = verifyOriginalV1ClaimBinding(profile, claim, pv, backend)
	}
	if !admitted || benchmarkIdentity(claim) != original.Claim || !retained ||
		!umamiRetainedClaimMetadata(claim, original.Consumer.Name) {
		return nil, nil, errors.New("the original ownerless Umami claim is not bound to the witnessed Retain PV")
	}
	_, err = j.read(ctx)
	return job, claim, err
}

func completedUmamiReusedJoin(job, cluster object, original cnpgStandbyCutover, identity benchmarkResource) bool {
	if !recoveredNativeObject(job, "batch/v1", "Job", original.Consumer.Namespace) || benchmarkIdentity(job) != identity ||
		!cnpgOwned(job, cluster) || text(job, "metadata", "labels", "cnpg.io/jobRole") != "join" ||
		text(job, "metadata", "labels", "cnpg.io/instanceName") != original.Consumer.Name ||
		text(job, "spec", "template", "spec", "restartPolicy") != "Never" ||
		value(job, "status", "succeeded") != float64(1) {
		return false
	}
	for _, key := range []string{"active", "failed", "ready", "terminating"} {
		if n := value(job, "status", key); n != nil && n != float64(0) {
			return false
		}
	}
	template := valueObject(job, "spec", "template")
	return cnpgCondition(job, "Complete") && podUsesUmamiClaim(template, original.Claim.Name)
}

func completedUmamiJoinPod(pod object, join benchmarkResource) bool {
	if !recoveredNativeObject(pod, "v1", "Pod", join.Namespace) ||
		text(pod, "status", "phase") != "Succeeded" || text(pod, "spec", "restartPolicy") != "Never" {
		return false
	}
	owners, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 {
		return false
	}
	owner, _ := owners[0].(object)
	return text(owner, "apiVersion") == "batch/v1" && text(owner, "kind") == "Job" &&
		text(owner, "name") == join.Name && text(owner, "uid") == join.UID && value(owner, "controller") == true
}

func podUsesUmamiClaim(pod object, claim string) bool {
	mounts, _ := value(pod, "spec", "volumes").([]any)
	for _, raw := range mounts {
		mount, _ := raw.(object)
		if text(mount, "persistentVolumeClaim", "claimName") == claim {
			return true
		}
	}
	return false
}

func readDetachedUmamiBackend(ctx context.Context, c client, original cnpgStandbyCutover, phase string) (object, object, error) {
	if original.Provider != nil {
		pv, err := readDetachedProviderCNPGOriginal(ctx, c, *original.Provider, phase)
		return pv, nil, err
	}
	pv, err := c.get(ctx, "persistentvolume", "", original.PV.Name, false)
	if err != nil {
		return nil, nil, err
	}
	backend, err := c.get(ctx, "volumes.longhorn.io", original.Volume.Namespace, original.Volume.Name, false)
	if err != nil {
		return nil, nil, err
	}
	if benchmarkIdentity(pv) != original.PV || benchmarkIdentity(backend) != original.Volume ||
		verifyBenchmarkDetachedBackendState(backend) != nil {
		return nil, nil, errors.New("the original Umami backend is not demonstrably detached")
	}
	native, err := c.get(ctx, "volumeattachments.longhorn.io", original.Volume.Namespace, original.Volume.Name, false)
	if err != nil || !recoveredNativeObject(native, "longhorn.io/v1beta2", "VolumeAttachment", original.Volume.Namespace) ||
		!recoveredVolumeOwner(native, backend) || text(native, "spec", "volume") != original.Volume.Name ||
		!absentOrEmptyMap(value(native, "spec", "attachmentTickets")) ||
		!absentOrEmptyMap(value(native, "status", "attachmentTicketStatuses")) {
		return nil, nil, errors.New("the original Umami backend has unverified native attachment tickets")
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return nil, nil, err
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == original.PV.Name {
			return nil, nil, errors.New("the original Umami PV still has a CSI attachment")
		}
	}
	return pv, backend, nil
}

func umamiReleasedVolumeMatches(original cnpgStandbyCutover, pv, backend object) bool {
	profile, admitted := cnpgStandbyProfileForSource(cnpgSource{Namespace: original.Database.Namespace, Name: original.Database.Name})
	originalPVC := text(backend, "status", "kubernetesStatus", "pvcName")
	lastReference := text(backend, "status", "kubernetesStatus", "lastPVCRefAt")
	_, historical := time.Parse(time.RFC3339, lastReference)
	return admitted && recoveredNativeObject(pv, "v1", "PersistentVolume", "") && benchmarkIdentity(pv) == original.PV &&
		text(pv, "status", "phase") == "Released" && text(pv, "spec", "persistentVolumeReclaimPolicy") == "Retain" &&
		text(pv, "spec", "claimRef", "name") == original.Claim.Name &&
		text(pv, "spec", "claimRef", "namespace") == original.Claim.Namespace &&
		text(pv, "spec", "claimRef", "uid") == original.Claim.UID &&
		text(pv, "spec", "storageClassName") == "longhorn-wffc" && providerRWOFilesystem(pv) &&
		text(pv, "spec", "capacity", "storage") == profile.Size &&
		text(pv, "spec", "csi", "driver") == "driver.longhorn.io" && text(pv, "spec", "csi", "volumeHandle") == original.Volume.Name &&
		recoveredNativeObject(backend, "longhorn.io/v1beta2", "Volume", original.Volume.Namespace) &&
		benchmarkIdentity(backend) == original.Volume && text(backend, "spec", "dataEngine") == "v1" &&
		text(backend, "spec", "size") == profile.SizeBytes && verifyBenchmarkDetachedBackendState(backend) == nil &&
		value(backend, "spec", "numberOfReplicas") == float64(3) &&
		(text(backend, "status", "robustness") == "healthy" || text(backend, "status", "robustness") == "unknown") &&
		text(backend, "status", "kubernetesStatus", "pvName") == original.PV.Name &&
		(originalPVC == "" || originalPVC == original.Claim.Name && historical == nil &&
			text(backend, "status", "kubernetesStatus", "namespace") == original.Claim.Namespace)
}

func observeUmamiReleasedOriginal(ctx context.Context, j *umamiStandbyReplacementJournal, requireEmpty bool) error {
	c, original := j.c, j.record.Original
	_, _, err := observeUmamiUnchangedPeers(ctx, j)
	if err != nil {
		return err
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", original.Claim.Namespace, original.Claim.Name, true)
	if err != nil {
		return err
	}
	if claim != nil && (requireEmpty || benchmarkIdentity(claim) == original.Claim ||
		text(claim, "spec", "volumeName") == original.PV.Name || text(claim, "spec", "storageClassName") != "longhorn-v2") {
		return errors.New("the released Umami name is not empty or bound exclusively to fresh V2 storage")
	}
	pv, backend, err := readDetachedUmamiBackend(ctx, c, original, "Released")
	if err != nil {
		return err
	}
	if original.Provider == nil && !umamiReleasedVolumeMatches(original, pv, backend) {
		return errors.New("the old Umami PV is not a demonstrably released Retain volume")
	}
	_, err = j.read(ctx)
	return err
}
