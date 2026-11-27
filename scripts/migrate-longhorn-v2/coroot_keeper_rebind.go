package main

import (
	"errors"
	"reflect"
	"strings"
)

const (
	corootKeeperStagingPrefix = "coroot-keeper-v2-data-"
	corootKeeperDataSize      = "2Gi"
	corootKeeperDataBytes     = "2147483648"
)

var errCorootKeeperRebind = errors.New("the retained Keeper source and prepared V2 destination are unverified")

type corootKeeperRebind struct {
	Coroot                benchmarkResource
	Original              corootKeeperProviderDisk
	StagedClaim, StagedPV benchmarkResource
	StagedVolume          benchmarkResource
}

func (r corootKeeperRebind) replacementClaim(coroot, original, staged, backend object) (object, error) {
	owner, err := r.retainedBindings(coroot, original, staged, backend)
	if err != nil {
		return nil, err
	}
	return object{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{
			"name": r.Original.Claim.Name, "namespace": corootKeeperNamespace,
			"labels": corootKeeperSelector(), "ownerReferences": []any{owner},
		},
		"spec": object{
			"volumeName": r.StagedPV.Name, "storageClassName": "longhorn-v2", "accessModes": []any{"ReadWriteOnce"},
			"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": corootKeeperDataSize}},
		},
	}, nil
}

// The caller submits this through the normal UID and resource-version patch
// fence. The original provider PV is only inspected and retains its old claim.
func (r corootKeeperRebind) rebindChanges(coroot, original, staged, backend, replacement object) ([]object, error) {
	desired, err := r.replacementClaim(coroot, original, staged, backend)
	if err != nil {
		return nil, err
	}
	uid := text(replacement, "metadata", "uid")
	if !recoveredNativeObject(replacement, "v1", "PersistentVolumeClaim", corootKeeperNamespace) ||
		text(replacement, "metadata", "name") != r.Original.Claim.Name ||
		uid == r.Original.Claim.UID || uid == r.StagedClaim.UID ||
		!reflect.DeepEqual(value(replacement, "spec"), value(desired, "spec")) ||
		!reflect.DeepEqual(value(replacement, "metadata", "ownerReferences"), value(desired, "metadata", "ownerReferences")) ||
		text(replacement, "status", "phase") != "Pending" {
		return nil, errCorootKeeperRebind
	}
	return []object{
		{"op": "test", "path": "/spec/claimRef/uid", "value": r.StagedClaim.UID},
		{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		{"op": "replace", "path": "/spec/claimRef", "value": object{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": corootKeeperNamespace,
			"name": r.Original.Claim.Name, "uid": uid,
		}},
	}, nil
}

func (r corootKeeperRebind) retainedBindings(coroot, original, staged, backend object) (object, error) {
	if !recoveredNativeObject(coroot, "coroot.com/v1", "Coroot", corootKeeperNamespace) ||
		benchmarkIdentity(coroot) != r.Coroot || r.Coroot.Name != "coroot" ||
		!keeperReleasedPV(original, r.Original.PV, r.Original.Claim, "hcloud", hetznerCSIDriver, r.Original.Handle) ||
		!keeperReleasedPV(staged, r.StagedPV, r.StagedClaim, "longhorn-v2", "driver.longhorn.io", r.StagedVolume.Name) ||
		!recoveredNativeObject(backend, "longhorn.io/v1beta2", "Volume", "longhorn-system") ||
		benchmarkIdentity(backend) != r.StagedVolume || r.StagedPV.UID == r.Original.PV.UID ||
		r.StagedPV.Name == r.Original.PV.Name || !strings.HasPrefix(r.StagedClaim.Name, corootKeeperStagingPrefix) ||
		r.StagedClaim.UID == r.Original.Claim.UID || r.StagedVolume.Name != r.StagedPV.Name ||
		r.StagedPV.Name != "pvc-"+r.StagedClaim.UID ||
		text(staged, "spec", "capacity", "storage") != corootKeeperDataSize ||
		text(backend, "spec", "dataEngine") != "v2" || text(backend, "spec", "size") != corootKeeperDataBytes ||
		value(backend, "spec", "numberOfReplicas") != float64(3) ||
		text(backend, "status", "state") != "detached" || text(backend, "spec", "nodeID") != "" ||
		text(backend, "status", "currentNodeID") != "" {
		return nil, errCorootKeeperRebind
	}
	return object{
		"apiVersion": "coroot.com/v1", "kind": "Coroot", "name": r.Coroot.Name, "uid": r.Coroot.UID,
		"controller": true, "blockOwnerDeletion": true,
	}, nil
}

func keeperReleasedPV(observed object, identity, claim benchmarkResource, class, driver, handle string) bool {
	return text(observed, "status", "phase") == "Released" && keeperRetainedPVBinding(observed, identity, claim, class, driver, handle)
}

func keeperRetainedPVBinding(observed object, identity, claim benchmarkResource, class, driver, handle string) bool {
	return recoveredNativeObject(observed, "v1", "PersistentVolume", "") &&
		benchmarkIdentity(observed) == identity && claim.Namespace == corootKeeperNamespace && isolationUID.MatchString(claim.UID) &&
		text(observed, "spec", "persistentVolumeReclaimPolicy") == "Retain" &&
		text(observed, "spec", "storageClassName") == class && providerRWOFilesystem(observed) &&
		text(observed, "spec", "csi", "driver") == driver && text(observed, "spec", "csi", "volumeHandle") == handle &&
		text(observed, "spec", "claimRef", "uid") == claim.UID && text(observed, "spec", "claimRef", "name") == claim.Name &&
		text(observed, "spec", "claimRef", "namespace") == claim.Namespace
}
