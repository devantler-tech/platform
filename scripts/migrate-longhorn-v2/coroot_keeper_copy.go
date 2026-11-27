package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"
)

const corootKeeperCopyWaiting = "COROOT_KEEPER_OFFLINE_COPY=WAITING_FOR_ISOLATION"

var errCorootKeeperCopy = errors.New("the isolated Keeper data copy is unverified")

func (j *corootKeeperJournal) copyVolume(ctx context.Context, disks []cnpgRecoveryDiskBinding) error {
	r := j.record
	member := r.Before.Members[r.Input.Ordinal]
	policy, err := j.createKeeperAsset(ctx, "copy-isolation-created")
	if err != nil {
		return err
	}
	gate, err := j.createKeeperAsset(ctx, "copy-gate-created")
	if err != nil {
		return err
	}
	desiredPod := corootKeeperCopyPod(r.Run, r.Source, r.Rebind.Original.Claim.Name, member.Node)
	created, err := j.createKeeperAsset(ctx, "copy-pod-created")
	if err != nil {
		return err
	}
	r = j.record
	proof, err := j.releaseCopyGate(ctx, r, member, created, desiredPod, policy, disks)
	if err != nil {
		return err
	}
	r.Phase = "copy-permitted"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	// The projected gate and the verified copy both need time after the API patch.
	err = waitReadyWithin(ctx, 6*time.Minute, func(ctx context.Context) error {
		pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, r.CopyPod.Name, false)
		if err != nil {
			return errCorootKeeperCopy
		}
		if !j.c.matchBoundKeeperVolumePod(ctx, pod, created, desiredPod, member.Node) {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		if text(pod, "status", "phase") == "Failed" {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		if text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
			value(corootKeeperCopyStatus(pod), "state", "terminated", "exitCode") != float64(0) ||
			!corootKeeperCopyLog(ctx, j.c, r.CopyPod.Name, corootKeeperCopyWaiting+"\n"+corootKeeperCopyVerified) {
			return errCorootKeeperCopy
		}
		after, err := corootKeeperCopyIsolation.observe(ctx, j.c, pod, policy, r.Run, member.Node)
		if err != nil || !sameStorageCopyIsolation(proof, after) {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		return j.verifyCopyInputs(ctx, r, member, disks)
	})
	if err != nil {
		return err
	}
	r.Phase = "copy-bytes-verified"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	if err := removeScratch(ctx, j.c, r.Source, "pod", corootKeeperNamespace,
		"/api/v1/namespaces/"+corootKeeperNamespace+"/pods/"+r.CopyPod.Name, created); err != nil {
		return err
	}
	if err := waitReadyWithin(ctx, 2*time.Minute, func(ctx context.Context) error {
		return verifyCorootKeeperDetach(ctx, j.c, r.Rebind, true)
	}); err != nil {
		return err
	}
	if err := removeScratch(ctx, j.c, r.Source, "configmap", corootKeeperNamespace,
		"/api/v1/namespaces/"+corootKeeperNamespace+"/configmaps/"+r.CopyGate.Name, gate); err != nil {
		return err
	}
	if err := removeScratch(ctx, j.c, r.Source, "ciliumnetworkpolicy.cilium.io", corootKeeperNamespace,
		"/apis/cilium.io/v2/namespaces/"+corootKeeperNamespace+"/ciliumnetworkpolicies/"+r.CopyIsolation.Name, policy); err != nil {
		return err
	}
	r.Phase = "copied-volumes-detached"
	return j.advance(ctx, r)
}

func (j *corootKeeperJournal) releaseCopyGate(ctx context.Context, r corootKeeperCutoverRecord, member corootKeeperMember,
	created, desired, policy object, disks []cnpgRecoveryDiskBinding,
) (recoveryCiliumDatapath, error) {
	var proof recoveryCiliumDatapath
	err := waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, r.CopyPod.Name, false)
		if err != nil {
			return errCorootKeeperCopy
		}
		if !j.c.matchBoundKeeperVolumePod(ctx, pod, created, desired, member.Node) {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		if text(pod, "status", "phase") != "Pending" ||
			text(corootKeeperCopyStatus(pod), "state", "running", "startedAt") == "" ||
			!corootKeeperCopyLog(ctx, j.c, r.CopyPod.Name, corootKeeperCopyWaiting) {
			return errCorootKeeperCopy
		}
		if err := j.verifyCopyInputs(ctx, r, member, disks); err != nil {
			return err
		}
		observed, err := corootKeeperCopyIsolation.observe(ctx, j.c, pod, policy, r.Run, member.Node)
		if err != nil {
			return err
		}
		current, err := j.c.get(ctx, "pod", corootKeeperNamespace, r.CopyPod.Name, false)
		if err != nil || !j.c.matchBoundKeeperVolumePod(ctx, current, created, desired, member.Node) ||
			text(current, "status", "podIP") != text(pod, "status", "podIP") ||
			text(current, "spec", "nodeName") != member.Node || text(current, "status", "phase") != "Pending" {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		gate, err := j.c.get(ctx, "configmap", corootKeeperNamespace, r.CopyGate.Name, false)
		if err != nil || benchmarkIdentity(gate) != r.CopyGate || !verifyCorootKeeperGate(gate, r, "hold") {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		if err := sourceCurrent(ctx, j.c.run, r.Source); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		if err := j.c.patch(ctx, "configmap", corootKeeperNamespace, gate,
			object{"op": "test", "path": "/data/state", "value": "hold"},
			object{"op": "replace", "path": "/data/state", "value": r.Run + ":" + r.Source}); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		released, err := j.c.get(ctx, "configmap", corootKeeperNamespace, r.CopyGate.Name, false)
		if err != nil || benchmarkIdentity(released) != r.CopyGate || !verifyCorootKeeperGate(released, r, r.Run+":"+r.Source) {
			return errors.Join(errReadinessTerminal, errCorootKeeperCopy)
		}
		proof = observed
		return nil
	})
	return proof, err
}

func (j *corootKeeperJournal) verifyCopyInputs(ctx context.Context, r corootKeeperCutoverRecord, member corootKeeperMember, disks []cnpgRecoveryDiskBinding) error {
	if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
		return err
	}
	stateful, err := j.c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || benchmarkIdentity(stateful) != r.Before.Controller {
		return errCorootKeeperCopy
	}
	src := r.Rebind.Original
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, src.Claim.Name, false)
	if err != nil {
		return err
	}
	original, err := j.c.get(ctx, "persistentvolume", "", src.PV.Name, false)
	if err != nil {
		return err
	}
	attachments, err := j.c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	observed, err := bindCorootKeeperProviderDisk(member, stateful, claim, original, attachments)
	if err != nil || observed.Claim != src.Claim || observed.PV != src.PV || observed.Digest != src.Digest {
		return errCorootKeeperCopy
	}
	target, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.StagedClaim.Name, false)
	if err != nil || benchmarkIdentity(target) != r.StagedClaim {
		return errCorootKeeperCopy
	}
	pv, volume, err := readCorootKeeperV2Attachment(ctx, j.c, target, member.Node, disks)
	if err != nil || pv != r.Rebind.StagedPV || volume != r.Rebind.StagedVolume {
		return errCorootKeeperCopy
	}
	return nil
}

func corootKeeperGate(record corootKeeperCutoverRecord, state string) object {
	return object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": corootKeeperCopyGatePrefix + record.Run, "namespace": corootKeeperNamespace,
			"annotations": object{corootKeeperFenceOwner: record.Run + ":" + record.Source}},
		"data": object{"state": state}}
}

func verifyCorootKeeperGate(gate object, record corootKeeperCutoverRecord, state string) bool {
	desired := corootKeeperGate(record, state)
	return recoveredNativeObject(gate, "v1", "ConfigMap", corootKeeperNamespace) &&
		text(gate, "metadata", "name") == text(desired, "metadata", "name") &&
		value(gate, "metadata", "ownerReferences") == nil && value(gate, "binaryData") == nil &&
		reflect.DeepEqual(value(gate, "metadata", "annotations"), value(desired, "metadata", "annotations")) &&
		reflect.DeepEqual(value(gate, "data"), value(desired, "data"))
}

func corootKeeperCopyStatus(pod object) object {
	init, _ := value(pod, "status", "initContainerStatuses").([]any)
	if len(init) != 1 {
		return nil
	}
	status, _ := init[0].(object)
	if text(status, "name") != "copy" || text(status, "imageID") != corootKeeperImageID ||
		value(status, "restartCount") != float64(0) {
		return nil
	}
	return status
}

func corootKeeperCopyLog(ctx context.Context, c client, pod, expected string) bool {
	out, err := c.kubectl(ctx, nil, "logs", pod, "--namespace="+corootKeeperNamespace, "--container=copy")
	return err == nil && strings.TrimSpace(string(out)) == expected
}
