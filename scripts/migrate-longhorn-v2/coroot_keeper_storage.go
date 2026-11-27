package main

import (
	"errors"
	"reflect"
)

var errCorootKeeperStorage = errors.New("the original Coroot Keeper provider disk and its sole CSI attachment are unverified")

type corootKeeperProviderDisk struct {
	Claim, PV, Ticket benchmarkResource
	Handle, Digest    string
}

func bindCorootKeeperProviderDisk(member corootKeeperMember, controller, claim, pv object, attachments []object) (corootKeeperProviderDisk, error) {
	var binding corootKeeperProviderDisk
	claimName := "data-" + member.Pod
	claimUID, pvName := text(claim, "metadata", "uid"), text(pv, "metadata", "name")
	handle := text(pv, "spec", "csi", "volumeHandle")
	owners, _ := value(controller, "metadata", "ownerReferences").([]any)
	if member.Pod == "" || member.Node == "" || len(owners) != 1 ||
		!reflect.DeepEqual(owners, value(claim, "metadata", "ownerReferences")) {
		return binding, errCorootKeeperStorage
	}
	owner, _ := owners[0].(object)
	if text(owner, "apiVersion") != "coroot.com/v1" || text(owner, "kind") != "Coroot" ||
		text(owner, "name") != "coroot" || value(owner, "controller") != true || !isolationUID.MatchString(text(owner, "uid")) {
		return binding, errCorootKeeperStorage
	}
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", corootKeeperNamespace) ||
		text(claim, "metadata", "name") != claimName || text(claim, "status", "phase") != "Bound" ||
		text(claim, "spec", "volumeName") != pvName || text(claim, "spec", "storageClassName") != "hcloud" ||
		text(claim, "spec", "resources", "requests", "storage") != "2Gi" {
		return binding, errCorootKeeperStorage
	}
	if !recoveredNativeObject(pv, "v1", "PersistentVolume", "") || pvName != "pvc-"+claimUID ||
		text(pv, "status", "phase") != "Bound" || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		text(pv, "spec", "storageClassName") != "hcloud" || text(pv, "spec", "capacity", "storage") != "10Gi" ||
		text(pv, "spec", "csi", "fsType") != "ext4" {
		return binding, errCorootKeeperStorage
	}
	provider, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	if err != nil || len(provider) != 1 || provider[0].Claim != claimName || provider[0].Namespace != corootKeeperNamespace {
		return binding, errCorootKeeperStorage
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") != pvName {
			continue
		}
		if binding.Ticket.UID != "" || !recoveredNativeObject(attachment, "storage.k8s.io/v1", "VolumeAttachment", "") ||
			text(attachment, "spec", "attacher") != hetznerCSIDriver || text(attachment, "spec", "nodeName") != member.Node ||
			value(attachment, "status", "attached") != true || value(attachment, "status", "attachError") != nil ||
			value(attachment, "status", "detachError") != nil {
			return corootKeeperProviderDisk{}, errCorootKeeperStorage
		}
		binding.Ticket = benchmarkIdentity(attachment)
	}
	if binding.Ticket.UID == "" {
		return binding, errCorootKeeperStorage
	}
	binding.Claim, binding.PV, binding.Handle = benchmarkIdentity(claim), benchmarkIdentity(pv), handle
	binding.Digest = providerRetentionDigest(provider[0])
	return binding, nil
}
