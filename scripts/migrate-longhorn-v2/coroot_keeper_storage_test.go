package main

import (
	"testing"
)

func TestBindCorootKeeperProviderDisk(t *testing.T) {
	const claimUID = "31915a8b-321e-4c0c-a6bc-cc91baf16b4b"
	const volumeUID = "456a6700-9888-44f4-8e30-aeab136f0352"
	const ownerUID = "3741188f-892b-4881-866e-a2a4293932bb"
	const attachmentUID = "cbefa334-ec63-4eb7-abcd-f4ddbb265325"
	member := corootKeeperMember{Pod: corootKeeperName + "-1", Node: "worker"}
	claimName, volumeName := "data-"+member.Pod, "pvc-"+claimUID
	owner := object{"apiVersion": "coroot.com/v1", "name": "coroot", "kind": "Coroot", "controller": true, "uid": ownerUID}
	stateful := object{"metadata": object{"ownerReferences": []any{owner}}}
	claim := object{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{"name": claimName, "namespace": corootKeeperNamespace, "uid": claimUID, "resourceVersion": "10", "ownerReferences": []any{owner}},
		"spec": object{"volumeName": volumeName, "storageClassName": "hcloud", "accessModes": []any{"ReadWriteOnce"},
			"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": "2Gi"}}},
		"status": object{"phase": "Bound", "capacity": object{"storage": "10Gi"}},
	}
	pv := object{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": object{"name": volumeName, "uid": volumeUID, "resourceVersion": "20"},
		"spec": object{"storageClassName": "hcloud", "persistentVolumeReclaimPolicy": "Retain", "volumeMode": "Filesystem",
			"accessModes": []any{"ReadWriteOnce"}, "capacity": object{"storage": "10Gi"},
			"csi":      object{"driver": "csi.hetzner.cloud", "fsType": "ext4", "volumeHandle": "916230"},
			"claimRef": object{"name": claimName, "namespace": corootKeeperNamespace, "uid": claimUID}},
		"status": object{"phase": "Bound"},
	}
	ticket := object{
		"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment",
		"metadata": object{"name": "csi-valid", "uid": attachmentUID, "resourceVersion": "30"},
		"spec":     object{"source": object{"persistentVolumeName": volumeName}, "nodeName": member.Node, "attacher": "csi.hetzner.cloud"},
		"status":   object{"attached": true},
	}
	got, err := bindCorootKeeperProviderDisk(member, stateful, claim, pv, []object{ticket})
	if err != nil || got.Claim.Name != claimName || got.PV.UID != volumeUID || got.Ticket.UID != attachmentUID || got.Handle != "916230" {
		t.Fatalf("native bound provider disk rejected: %v", err)
	}
	for _, tt := range []struct {
		name      string
		target    object
		key       string
		bad       any
		duplicate bool
	}{
		{name: "wrong CSI host", target: valueObject(ticket, "spec"), key: "nodeName", bad: "different"},
		{name: "duplicate CSI attachment", duplicate: true},
		{name: "non-retained PV", target: valueObject(pv, "spec"), key: "persistentVolumeReclaimPolicy", bad: "Delete"},
		{name: "mismatched claimRef UID", target: valueObject(pv, "spec", "claimRef"), key: "uid", bad: ownerUID},
		{name: "claim has different operator owner", target: valueObject(claim, "metadata"), key: "ownerReferences", bad: []any{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attachments := []object{ticket}
			if tt.duplicate {
				attachments = append(attachments, ticket)
			} else {
				original := tt.target[tt.key]
				tt.target[tt.key] = tt.bad
				defer func() { tt.target[tt.key] = original }()
			}
			if _, err := bindCorootKeeperProviderDisk(member, stateful, claim, pv, attachments); err == nil {
				t.Fatal("unverified original storage accepted")
			}
		})
	}
}
