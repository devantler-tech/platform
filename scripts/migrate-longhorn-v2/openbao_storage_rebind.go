package main

import (
	"reflect"
	"regexp"
	"strconv"
)

const openBaoVolumeRebindOwner = "storage.platform.devantler.tech/openbao-rebind"

var openBaoMemberClaim = regexp.MustCompile(`^(data|audit)-openbao-[012]$`)

type openBaoStorageRebind struct {
	Source                                         openBaoProviderDisk
	StagingClaim, DestinationPV, DestinationVolume benchmarkResource
	Run, SourceSHA                                 string
}

type openBaoReboundStorage struct {
	Intent                            openBaoStorageRebind
	Original, Dest, Backend, NewClaim object
}

// A PV may already point at the journaled successor while the claim's status
// still reports Pending. Leave that binding alone and wait for the controller.
func (o openBaoReboundStorage) rebindStep(expected benchmarkResource) (bool, []object, error) {
	r, claim := o.Intent, o.NewClaim
	if !r.valid() || benchmarkIdentity(claim) != expected || !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", openBaoNamespace) {
		return false, nil, errOpenBaoProviderStorage
	}
	if !openBaoRetainedPVBinding(o.Dest, r.DestinationPV, expected, "longhorn-v2", "driver.longhorn.io", r.DestinationVolume.Name, r.Source.Bytes) {
		patch, err := r.patch(o.Original, o.Dest, o.Backend, claim)
		return false, patch, err
	}
	desired := r.replacementClaim()
	if expected.Name != r.Source.PVC.Name || expected.UID == r.Source.PVC.UID || expected.UID == r.StagingClaim.UID ||
		!openBaoRetainedReleasedPV(o.Original, r.Source.PV, r.Source.PVC, "hcloud", hetznerCSIDriver, r.Source.ProviderHandle, r.Source.Bytes) ||
		!reflect.DeepEqual(value(claim, "spec"), value(desired, "spec")) ||
		text(claim, "metadata", "annotations", openBaoVolumeRebindOwner) != text(desired, "metadata", "annotations", openBaoVolumeRebindOwner) ||
		!r.detachedV2Destination(o.Backend) {
		return false, nil, errOpenBaoProviderStorage
	}
	phase := text(o.Dest, "status", "phase")
	switch text(claim, "status", "phase") {
	case "Pending":
		if phase == "Released" || phase == "Bound" {
			return false, nil, nil
		}
	case "Bound":
		if phase == "Bound" && providerVolumeBytes(text(claim, "status", "capacity", "storage")) == r.Source.Bytes {
			return true, nil, nil
		}
	}
	return false, nil, errOpenBaoProviderStorage
}

func (r openBaoStorageRebind) replacementClaim() object {
	size := strconv.FormatUint(r.Source.Bytes>>30, 10) + "Gi"
	return object{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{
			"name": r.Source.PVC.Name, "namespace": openBaoNamespace,
			"annotations": object{openBaoVolumeRebindOwner: r.Run + ":" + r.SourceSHA + ":" + r.Source.PVC.UID},
		},
		"spec": object{
			"volumeName": r.DestinationPV.Name, "storageClassName": "longhorn-v2", "volumeMode": "Filesystem",
			"accessModes": []any{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": size}},
		},
	}
}

func (r openBaoStorageRebind) valid() bool {
	name := openBaoMemberClaim.FindStringSubmatch(r.Source.PVC.Name)
	if len(name) != 2 {
		return false
	}
	size := uint64(10 << 30)
	if name[1] == "audit" {
		size = 40 << 30
	}
	return r.Source.Bytes == size && validBenchmarkOwner(r.Run, r.SourceSHA) && r.Source.PVC != r.StagingClaim &&
		r.Source.PV != r.DestinationPV && r.StagingClaim.Name == "lh-openbao-staging-"+name[1]+"-"+r.Run &&
		r.StagingClaim.Namespace == openBaoNamespace && isolationUID.MatchString(r.StagingClaim.UID) &&
		r.DestinationPV.Name == "pvc-"+r.StagingClaim.UID && r.DestinationVolume.Name == r.DestinationPV.Name
}

// The controller releases both retained claims before it submits either
// guarded rebind. The caller keeps the member fence through both bindings.
func (r openBaoStorageRebind) patch(original, staged, volume, replacement object) ([]object, error) {
	if !r.valid() ||
		!openBaoRetainedReleasedPV(original, r.Source.PV, r.Source.PVC, "hcloud", hetznerCSIDriver, r.Source.ProviderHandle, r.Source.Bytes) ||
		!openBaoRetainedReleasedPV(staged, r.DestinationPV, r.StagingClaim, "longhorn-v2", "driver.longhorn.io", r.DestinationVolume.Name, r.Source.Bytes) ||
		!r.detachedV2Destination(volume) {
		return nil, errOpenBaoProviderStorage
	}
	desired := r.replacementClaim()
	uid := text(replacement, "metadata", "uid")
	if !recoveredNativeObject(replacement, "v1", "PersistentVolumeClaim", openBaoNamespace) ||
		text(replacement, "metadata", "name") != r.Source.PVC.Name || uid == r.Source.PVC.UID || uid == r.StagingClaim.UID ||
		text(replacement, "status", "phase") != "Pending" || value(replacement, "metadata", "ownerReferences") != nil ||
		!reflect.DeepEqual(value(replacement, "metadata", "annotations"), value(desired, "metadata", "annotations")) ||
		!reflect.DeepEqual(value(replacement, "spec"), value(desired, "spec")) {
		return nil, errOpenBaoProviderStorage
	}
	return []object{
		{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		{"op": "test", "path": "/spec/claimRef", "value": value(staged, "spec", "claimRef")},
		{"op": "replace", "path": "/spec/claimRef", "value": object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"namespace": openBaoNamespace, "name": r.Source.PVC.Name, "uid": uid}},
	}, nil
}

func (r openBaoStorageRebind) detachedV2Destination(volume object) bool {
	return recoveredNativeObject(volume, "longhorn.io/v1beta2", "Volume", "longhorn-system") &&
		benchmarkIdentity(volume) == r.DestinationVolume && text(volume, "spec", "dataEngine") == "v2" &&
		value(volume, "spec", "numberOfReplicas") == float64(3) &&
		text(volume, "spec", "size") == strconv.FormatUint(r.Source.Bytes, 10) && verifyBenchmarkDetachedBackendState(volume) == nil
}

func openBaoRetainedReleasedPV(pv object, identity, claim benchmarkResource, class, driver, handle string, size uint64) bool {
	return text(pv, "status", "phase") == "Released" && openBaoRetainedPVBinding(pv, identity, claim, class, driver, handle, size)
}

func openBaoRetainedPVBinding(pv object, identity, claim benchmarkResource, class, driver, handle string, size uint64) bool {
	return recoveredNativeObject(pv, "v1", "PersistentVolume", "") && benchmarkIdentity(pv) == identity &&
		claim.Namespace == openBaoNamespace && isolationUID.MatchString(claim.UID) &&
		providerRWOFilesystem(pv) &&
		text(pv, "spec", "persistentVolumeReclaimPolicy") == "Retain" && text(pv, "spec", "storageClassName") == class &&
		text(pv, "spec", "claimRef", "name") == claim.Name && text(pv, "spec", "claimRef", "namespace") == claim.Namespace &&
		text(pv, "spec", "claimRef", "uid") == claim.UID && text(pv, "spec", "csi", "driver") == driver &&
		handle != "" && text(pv, "spec", "csi", "volumeHandle") == handle &&
		providerVolumeBytes(text(pv, "spec", "capacity", "storage")) == size
}

// An interrupted patch may bind either claim first. The replica remains
// fenced until both exact destinations are Bound and both originals retained.
func verifyOpenBaoReboundPair(member string, pair [2]openBaoReboundStorage, successors [2]benchmarkResource) error {
	if !openBaoRebindPairMatches(member, [2]openBaoStorageRebind{pair[0].Intent, pair[1].Intent}) ||
		successors[0].UID == successors[1].UID {
		return errOpenBaoProviderStorage
	}
	for i, observed := range pair {
		bound, _, err := observed.rebindStep(successors[i])
		if err != nil || !bound {
			return errOpenBaoProviderStorage
		}
	}
	return nil
}

func openBaoRebindPairMatches(member string, pair [2]openBaoStorageRebind) bool {
	first, second := pair[0], pair[1]
	return knownOpenBaoMember(member) && first.valid() && second.valid() &&
		first.Source.PVC.Name == "data-"+member && second.Source.PVC.Name == "audit-"+member &&
		first.Source.ProviderHandle != second.Source.ProviderHandle && first.DestinationPV.Name != second.DestinationPV.Name &&
		first.DestinationPV.UID != second.DestinationPV.UID && first.DestinationVolume.UID != second.DestinationVolume.UID &&
		first.Run == second.Run && first.SourceSHA == second.SourceSHA
}
