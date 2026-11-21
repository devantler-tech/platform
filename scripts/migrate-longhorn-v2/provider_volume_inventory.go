package main

import (
	"errors"
	"reflect"
	"sort"
	"strconv"

	kresource "k8s.io/apimachinery/pkg/api/resource"
)

const hetznerCSIDriver = "csi.hetzner.cloud"

type providerVolumeClaim struct {
	Namespace, Claim, ClaimUID, ClaimVersion string
	PV, PVUID, PVVersion, Handle             string
	RequestedBytes                           uint64
}

// The inventory binds PVC and PV identities on both sides of the live usage
// read. It never treats a missing or released original as zero capacity.
func selectProviderVolumeClaims(pvs, claims []object) ([]providerVolumeClaim, error) {
	unverified := errors.New("provider volume inventory has incomplete or changed bindings")
	if pvs == nil || claims == nil {
		return nil, unverified
	}
	claimsByName := make(map[string]object, len(claims))
	for _, claim := range claims {
		key := text(claim, "metadata", "namespace") + "/" + text(claim, "metadata", "name")
		if claimsByName[key] != nil {
			return nil, unverified
		}
		claimsByName[key] = claim
	}
	var selected []providerVolumeClaim
	seenHandles := map[string]bool{}
	seenClaims := map[string]bool{}
	seenPVs := map[string]bool{}
	for _, pv := range pvs {
		if text(pv, "spec", "csi", "driver") != hetznerCSIDriver {
			continue
		}
		bound := providerVolumeClaim{
			Namespace: text(pv, "spec", "claimRef", "namespace"), Claim: text(pv, "spec", "claimRef", "name"),
			ClaimUID: text(pv, "spec", "claimRef", "uid"), PV: text(pv, "metadata", "name"),
			PVUID: text(pv, "metadata", "uid"), PVVersion: text(pv, "metadata", "resourceVersion"), Handle: text(pv, "spec", "csi", "volumeHandle"),
		}
		handle, err := strconv.ParseInt(bound.Handle, 10, 64)
		if err != nil || handle <= 0 || strconv.FormatInt(handle, 10) != bound.Handle || seenHandles[bound.Handle] ||
			bound.PVUID == "" || bound.PVVersion == "" || bound.ClaimUID == "" || seenClaims[bound.ClaimUID] || seenPVs[bound.PVUID] ||
			safeKubeRequest("persistentvolume", "", bound.PV, false) != nil || !kubeNamespace.MatchString(bound.Namespace) ||
			safeKubeRequest("persistentvolumeclaim", bound.Namespace, bound.Claim, false) != nil ||
			text(pv, "status", "phase") != "Bound" || value(pv, "metadata", "deletionTimestamp") != nil || !providerRWOFilesystem(pv) {
			return nil, unverified
		}
		claim := claimsByName[bound.Namespace+"/"+bound.Claim]
		bound.ClaimVersion = text(claim, "metadata", "resourceVersion")
		class := text(pv, "spec", "storageClassName")
		if bound.ClaimVersion == "" || text(claim, "metadata", "uid") != bound.ClaimUID || text(claim, "spec", "volumeName") != bound.PV ||
			text(claim, "status", "phase") != "Bound" || value(claim, "metadata", "deletionTimestamp") != nil ||
			class == "" || text(claim, "spec", "storageClassName") != class || !providerRWOFilesystem(claim) {
			return nil, unverified
		}
		capacity := providerVolumeBytes(text(pv, "spec", "capacity", "storage"))
		requested := providerVolumeBytes(text(claim, "spec", "resources", "requests", "storage"))
		current := providerVolumeBytes(text(claim, "status", "capacity", "storage"))
		if capacity == 0 || requested == 0 || requested > capacity || current != capacity {
			return nil, unverified
		}
		bound.RequestedBytes = capacity
		seenHandles[bound.Handle], seenClaims[bound.ClaimUID], seenPVs[bound.PVUID] = true, true, true
		selected = append(selected, bound)
	}
	if len(selected) == 0 {
		return nil, errors.New("no provider-backed source volume was found")
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ClaimUID < selected[j].ClaimUID })
	return selected, nil
}

func providerRWOFilesystem(o object) bool {
	return text(o, "spec", "volumeMode") == "Filesystem" && reflect.DeepEqual(value(o, "spec", "accessModes"), []any{"ReadWriteOnce"})
}

func providerVolumeBytes(raw string) uint64 {
	quantity, err := kresource.ParseQuantity(raw)
	if err != nil {
		return 0
	}
	bytes, exact := quantity.AsInt64()
	if !exact || bytes <= 0 {
		return 0
	}
	return uint64(bytes)
}
