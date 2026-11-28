package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

func sameProviderCNPGOriginal(a, b *providerCNPGOriginal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func validProviderCNPGOriginal(witness cnpgStandbyCutover) bool {
	original := witness.Provider
	if original == nil || witness.Volume != (benchmarkResource{}) || original.Claim != witness.Claim || original.PV != witness.PV ||
		original.Attachment.Namespace != "" || !kubeName.MatchString(original.Attachment.Name) ||
		!isolationUID.MatchString(original.Attachment.UID) || !kubeNamespace.MatchString(original.Node) {
		return false
	}
	id, err := strconv.ParseInt(original.Handle, 10, 64)
	return err == nil && id > 0 && id < 1<<53 && strconv.FormatInt(id, 10) == original.Handle
}

func readAttachedProviderCNPGOriginal(ctx context.Context, c client, pod, claim, pv object) (providerCNPGOriginal, error) {
	if c.readProviderVolume == nil {
		return providerCNPGOriginal{}, errProviderCNPGStandby
	}
	tickets, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return providerCNPGOriginal{}, err
	}
	original, err := bindProviderCNPGOriginal(claim, pv, tickets, text(pod, "spec", "nodeName"))
	if err != nil {
		return providerCNPGOriginal{}, err
	}
	node, err := c.get(ctx, "node", "", original.Node, false)
	if err != nil {
		return providerCNPGOriginal{}, err
	}
	disk, err := c.readProviderVolume(ctx, original.Handle)
	if err != nil || !providerCNPGBackendMatches(original, disk) || !providerCNPGAttachedToNode(original, disk, node) {
		return providerCNPGOriginal{}, errProviderCNPGStandby
	}
	return original, nil
}

func providerCNPGAttachedToNode(original providerCNPGOriginal, provider, node object) bool {
	if !recoveredNativeObject(node, "v1", "Node", "") || !ready(valueObject(node, "status")) ||
		text(node, "metadata", "name") != original.Node {
		return false
	}
	raw, ok := strings.CutPrefix(text(node, "spec", "providerID"), "hcloud://")
	id, err := strconv.ParseInt(raw, 10, 64)
	return ok && err == nil && id > 0 && id < 1<<53 && strconv.FormatInt(id, 10) == raw &&
		text(provider, "assignee", "type") == "server" && value(provider, "assignee", "id") == float64(id) &&
		(provider["server"] == nil || provider["server"] == float64(id))
}

func verifyProviderCNPGDetached(original providerCNPGOriginal, phase string, pvs, attachments []object, provider object) error {
	refused := errors.New("the retained database volume is not demonstrably detached from the provider")
	if (phase != "Bound" && phase != "Released") || attachments == nil ||
		!providerCNPGBackendMatches(original, provider) {
		return refused
	}
	assignee, known := provider["assignee"]
	if !known || assignee != nil || provider["server"] != nil {
		return refused
	}
	matches := 0
	for _, pv := range pvs {
		sameName := text(pv, "metadata", "name") == original.PV.Name
		sameDisk := text(pv, "spec", "csi", "driver") == hetznerCSIDriver && text(pv, "spec", "csi", "volumeHandle") == original.Handle
		if !sameName && !sameDisk {
			continue
		}
		if !sameName || !sameDisk || !providerCNPGOriginalPVMatches(original, pv, phase) {
			return refused
		}
		matches++
	}
	if matches != 1 {
		return refused
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == original.PV.Name {
			return refused
		}
	}
	return nil
}

func readDetachedProviderCNPGOriginal(ctx context.Context, c client, original providerCNPGOriginal, phase string) (object, error) {
	if c.readProviderVolume == nil {
		return nil, errProviderCNPGStandby
	}
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return nil, err
	}
	tickets, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return nil, err
	}
	disk, err := c.readProviderVolume(ctx, original.Handle)
	if err != nil {
		return nil, err
	}
	if err := verifyProviderCNPGDetached(original, phase, pvs, tickets, disk); err != nil {
		return nil, err
	}
	return uniqueOpenBaoObject(pvs, original.PV.Name), nil
}

func providerCNPGOriginalPVMatches(original providerCNPGOriginal, pv object, phase string) bool {
	return recoveredNativeObject(pv, "v1", "PersistentVolume", "") && benchmarkIdentity(pv) == original.PV &&
		nativeRestoreUID(original.Claim.UID) && original.PV.Name == "pvc-"+original.Claim.UID &&
		text(pv, "status", "phase") == phase && text(pv, "spec", "persistentVolumeReclaimPolicy") == "Retain" &&
		text(pv, "spec", "storageClassName") == "hcloud" && providerRWOFilesystem(pv) &&
		text(pv, "spec", "csi", "driver") == hetznerCSIDriver && text(pv, "spec", "csi", "volumeHandle") == original.Handle &&
		providerVolumeBytes(text(pv, "spec", "capacity", "storage")) == 10*recoveryGiB &&
		text(pv, "spec", "claimRef", "uid") == original.Claim.UID &&
		text(pv, "spec", "claimRef", "namespace") == original.Claim.Namespace &&
		text(pv, "spec", "claimRef", "name") == original.Claim.Name
}

func providerCNPGOriginalClaimMatches(original providerCNPGOriginal, claim, pv object) bool {
	claims, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	return err == nil && len(claims) == 1 && benchmarkIdentity(claim) == original.Claim &&
		claims[0].Handle == original.Handle && claims[0].RequestedBytes == 10*recoveryGiB
}

func providerCNPGBackendMatches(original providerCNPGOriginal, provider object) bool {
	id, err := strconv.ParseInt(original.Handle, 10, 64)
	return err == nil && id > 0 && id < 1<<53 && strconv.FormatInt(id, 10) == original.Handle &&
		provider["id"] == float64(id) && provider["size"] == float64(10) && provider["status"] == "available" &&
		text(provider, "linux_device") == "/dev/disk/by-id/scsi-0HC_Volume_"+original.Handle &&
		text(provider, "labels", "managed-by") == "csi-driver" &&
		text(provider, "labels", "pv-name") == original.PV.Name &&
		text(provider, "labels", "pvc-name") == original.Claim.Name &&
		text(provider, "labels", "pvc-namespace") == original.Claim.Namespace
}
