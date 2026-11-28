package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var errCorootKeeperStaging = errors.New("the staged Keeper volume or its verified three-copy attachment changed")

func verifyKeeperCopyHostname(ctx context.Context, c client, name string) error {
	node, err := c.get(ctx, "node", "", name, false)
	if err != nil || !recoveredNativeObject(node, "v1", "Node", "") ||
		text(node, "metadata", "name") != name || text(node, "metadata", "labels", "kubernetes.io/hostname") != name ||
		!ready(valueObject(node, "status")) || value(node, "spec", "unschedulable") == true {
		return errCorootKeeperStaging
	}
	return nil
}

func (j *corootKeeperJournal) prepareVolume(ctx context.Context, diskBindings []cnpgRecoveryDiskBinding) error {
	r := j.record
	member := r.Before.Members[r.Input.Ordinal]
	desiredClaim := corootKeeperStagingClaim(r.Run, r.Source)
	if _, err := j.createKeeperAsset(ctx, "staging-claim-created"); err != nil {
		return err
	}
	desiredPod := corootKeeperStagePod(r.Run, r.Source, member.Node)
	pod, err := j.createKeeperAsset(ctx, "staging-pod-created")
	if err != nil {
		return err
	}
	r = j.record
	err = waitReadyWithin(ctx, 4*time.Minute, func(ctx context.Context) error {
		p, err := j.c.get(ctx, "pod", corootKeeperNamespace, r.StagePod.Name, false)
		if err != nil || !j.c.matchBoundKeeperVolumePod(ctx, p, pod, desiredPod, member.Node) || text(p, "spec", "nodeName") != member.Node ||
			text(p, "status", "phase") != "Running" || !ready(valueObject(p, "status")) {
			return errCorootKeeperStaging
		}
		currentClaim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.StagedClaim.Name, false)
		if err != nil || !matchCorootKeeperStageClaim(currentClaim, desiredClaim) || benchmarkIdentity(currentClaim) != r.StagedClaim ||
			text(currentClaim, "spec", "volumeName") != "pvc-"+r.StagedClaim.UID {
			return errCorootKeeperStaging
		}
		pv, volume, err := readCorootKeeperV2Attachment(ctx, j.c, currentClaim, member.Node, diskBindings)
		if err != nil {
			return err
		}
		out, err := j.c.kubectl(ctx, nil, "logs", r.StagePod.Name, "--namespace="+corootKeeperNamespace, "--container=verify")
		count, matched := strings.CutPrefix(strings.TrimSpace(string(out)), "COROOT_KEEPER_DESTINATION=READY bytes=")
		free, parseErr := strconv.ParseInt(count, 10, 64)
		if err != nil || !matched || parseErr != nil || free <= 64<<20 || free > 2<<30 {
			return errCorootKeeperStaging
		}
		r.StagedFreeBytes = free
		r.Rebind = corootKeeperRebind{
			Coroot: r.Coroot, Original: r.Before.Disks[r.Input.Ordinal],
			StagedClaim: r.StagedClaim, StagedPV: pv, StagedVolume: volume,
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.Phase = "staging-volume-verified"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	if err := removeScratch(ctx, j.c, r.Source, "pod", corootKeeperNamespace,
		"/api/v1/namespaces/"+corootKeeperNamespace+"/pods/"+r.StagePod.Name, pod); err != nil {
		return err
	}
	return waitReadyWithin(ctx, 2*time.Minute, func(ctx context.Context) error {
		return verifyCorootKeeperDetach(ctx, j.c, r.Rebind, false)
	})
}

func matchCorootKeeperStageClaim(current, desired object) bool {
	return recoveredNativeObject(current, "v1", "PersistentVolumeClaim", corootKeeperNamespace) &&
		text(current, "metadata", "name") == text(desired, "metadata", "name") &&
		text(current, "metadata", "annotations", corootKeeperFenceOwner) == text(desired, "metadata", "annotations", corootKeeperFenceOwner) &&
		value(current, "metadata", "ownerReferences") == nil && providerRWOFilesystem(current) &&
		text(current, "spec", "storageClassName") == "longhorn-v2" &&
		text(current, "spec", "resources", "requests", "storage") == corootKeeperDataSize &&
		value(current, "spec", "dataSource") == nil && value(current, "spec", "dataSourceRef") == nil
}

func matchCorootKeeperVolumePod(observed, created, desired object) bool {
	return matchStorageCopyVolumePod(observed, created, desired, corootKeeperNamespace, corootKeeperFenceOwner)
}

func matchStorageCopyVolumePod(observed, created, desired object, namespace, ownershipAnnotation string) bool {
	return recoveredNativeObject(observed, "v1", "Pod", namespace) &&
		benchmarkIdentity(observed) == benchmarkIdentity(created) &&
		matchStorageCopyVolumePodShape(observed, desired, namespace, ownershipAnnotation)
}

// A CREATE dry-run can prove admission defaults but has no persisted version.
func matchStorageCopyVolumePodShape(observed, desired object, namespace, ownershipAnnotation string) bool {
	if text(observed, "apiVersion") != "v1" || text(observed, "kind") != "Pod" ||
		text(observed, "metadata", "namespace") != namespace || value(observed, "metadata", "deletionTimestamp") != nil ||
		text(observed, "metadata", "name") != text(desired, "metadata", "name") ||
		value(observed, "metadata", "ownerReferences") != nil ||
		!reflect.DeepEqual(value(observed, "metadata", "labels"), value(desired, "metadata", "labels")) ||
		text(observed, "metadata", "annotations", ownershipAnnotation) != text(desired, "metadata", "annotations", ownershipAnnotation) ||
		value(observed, "spec", "hostNetwork") == true || value(observed, "spec", "hostPID") == true ||
		value(observed, "spec", "hostIPC") == true || value(observed, "spec", "ephemeralContainers") != nil {
		return false
	}
	for _, key := range []string{"affinity", "securityContext", "volumes", "automountServiceAccountToken", "enableServiceLinks", "hostUsers", "restartPolicy", "activeDeadlineSeconds"} {
		if !reflect.DeepEqual(value(observed, "spec", key), value(desired, "spec", key)) {
			return false
		}
	}
	for _, key := range []string{"containers", "initContainers"} {
		got, _ := value(observed, "spec", key).([]any)
		want, _ := value(desired, "spec", key).([]any)
		if len(got) != len(want) {
			return false
		}
		for i, expected := range want {
			have, _ := got[i].(object)
			spec, _ := expected.(object)
			for _, field := range []string{"name", "image", "command", "args", "env", "envFrom", "volumeMounts", "volumeDevices", "securityContext", "lifecycle"} {
				if !reflect.DeepEqual(have[field], spec[field]) {
					return false
				}
			}
		}
	}
	return true
}

func readCorootKeeperV2Attachment(ctx context.Context, c client, claim object, node string, disks []cnpgRecoveryDiskBinding) (benchmarkResource, benchmarkResource, error) {
	return readQualifiedV2Attachment(ctx, c, claim, node, corootKeeperNamespace, corootKeeperDataSize, corootKeeperDataBytes, disks, errCorootKeeperStaging)
}

func readQualifiedV2Attachment(ctx context.Context, c client, claim object, node, namespace, size, bytes string,
	disks []cnpgRecoveryDiskBinding, refused error) (benchmarkResource, benchmarkResource, error) {
	var zero benchmarkResource
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", namespace) || node == "" ||
		text(claim, "status", "phase") != "Bound" || text(claim, "status", "capacity", "storage") != size ||
		text(claim, "spec", "storageClassName") != "longhorn-v2" || !providerRWOFilesystem(claim) {
		return zero, zero, refused
	}
	name := text(claim, "spec", "volumeName")
	pv, err := c.get(ctx, "persistentvolume", "", name, false)
	if err != nil {
		return zero, zero, err
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", name, false)
	if err != nil {
		return zero, zero, err
	}
	engines, err := c.list(ctx, "engines.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(engines, "longhorn.io/v1beta2", "Engine", "longhorn-system") {
		return zero, zero, refused
	}
	var engine object
	for _, item := range engines {
		if recoveredVolumeAssociated(item, volume) {
			if engine != nil {
				return zero, zero, refused
			}
			engine = item
		}
	}
	if !recoveredPVBackend(claim, pv, volume, engine, namespace, name, size, bytes, node) {
		return zero, zero, refused
	}
	replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(replicas, "longhorn.io/v1beta2", "Replica", "longhorn-system") {
		return zero, zero, refused
	}
	if _, err := recoveredNativeCopies(disks, volume, engine, replicas, bytes); err != nil {
		return zero, zero, err
	}
	return benchmarkIdentity(pv), benchmarkIdentity(volume), nil
}

func verifyCorootKeeperDetach(ctx context.Context, c client, binding corootKeeperRebind, includeSource bool) error {
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", binding.StagedVolume.Name, false)
	if err != nil || benchmarkIdentity(volume) != binding.StagedVolume || verifyBenchmarkDetachedBackendState(volume) != nil {
		return errCorootKeeperStaging
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		backend := text(attachment, "spec", "source", "persistentVolumeName")
		if backend == binding.StagedPV.Name || includeSource && backend == binding.Original.PV.Name {
			return errCorootKeeperStaging
		}
	}
	return nil
}

func verifyExistingKeeperV2Member(ctx context.Context, c client, disks []cnpgRecoveryDiskBinding, ordinal int, member corootKeeperMember, claim object) error {
	cm, err := c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-coroot-keeper-"+strconv.Itoa(ordinal), false)
	var record corootKeeperCutoverRecord
	if err != nil || !recoveredNativeObject(cm, "v1", "ConfigMap", "longhorn-system") ||
		json.Unmarshal([]byte(text(cm, "data", "record")), &record) != nil || record.Phase != "replacement-verified" ||
		record.Input.Ordinal != ordinal || !validBenchmarkOwner(record.Run, record.Source) ||
		text(cm, "metadata", "annotations", corootKeeperFenceOwner) != record.Run+":"+record.Source ||
		record.NewClaim != benchmarkIdentity(claim) {
		return errCorootKeeperStaging
	}
	pv, volume, err := readCorootKeeperV2Attachment(ctx, c, claim, member.Node, disks)
	if err != nil || pv != record.Rebind.StagedPV || volume != record.Rebind.StagedVolume {
		return errCorootKeeperStaging
	}
	return nil
}
