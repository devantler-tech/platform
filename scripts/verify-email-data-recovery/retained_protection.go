package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// disposalProtectionBackup is the recovery point that protects both
// diagnostic copies while they are disposed. Each copy outlived the 24-hour
// backup it was restored from, and Velero has since collected both. The
// maintainer approved disposing of both copies on 2026-11-13 under this daily
// backup, which holds completed volume backups of the serving mailbox and of
// each copy and is retained until 2026-11-26. The name is fixed in reviewed
// code: no input, label or newer backup can select another protection point.
const disposalProtectionBackup = "velero-daily-full-20261112021735"

// protectionPoint binds a disposal to the fixed protection backup and to the
// copy's independently witnessed reader and claim identities.
type protectionPoint struct{ backup, readerUID, claimUID string }

// verifyProtection requires the fixed protection backup to remain valid past
// mustRemainUntil and to hold one completed, fully transferred volume backup
// of the serving mailbox claim and of this copy's reader claim.
func (d *recoveryDriver) verifyProtection(ctx context.Context, mustRemainUntil time.Time) error {
	fail := errors.New("protection-backup")
	p := d.protection
	if p.backup != disposalProtectionBackup || p.readerUID == "" || p.claimUID == "" || p.readerUID == p.claimUID || d.source.claimUID == "" || d.source.volume == "" {
		return fail
	}
	backup, found, err := d.lookup(ctx, "Backup", p.backup, "velero")
	if err != nil || !found {
		return fail
	}
	m, status := child(backup, "metadata"), child(backup, "status")
	expires, err := time.Parse(time.RFC3339, text(status, "expiration"))
	phase, uid := text(status, "phase"), text(m, "uid")
	// The daily backup is PartiallyFailed for unrelated namespaces; only the
	// two volume backups below establish this protection.
	if err != nil || uid == "" || text(m, "name") != p.backup || text(m, "namespace") != "velero" || m["deletionTimestamp"] != nil ||
		!expires.After(mustRemainUntil) || (phase != "Completed" && phase != "PartiallyFailed") {
		return fail
	}
	data, err := d.k.call(ctx, nil, "get", "podvolumebackups.velero.io", "--namespace=velero", "-o=json", "--selector=velero.io/backup-uid="+uid)
	if err != nil {
		return fail
	}
	var list object
	if json.Unmarshal(data, &list) != nil || text(child(list, "metadata"), "continue") != "" || child(list, "metadata")["remainingItemCount"] != nil {
		return fail
	}
	raw, ok := list["items"].([]any)
	volumes := items(list, "items")
	if !ok || len(raw) != len(volumes) || len(volumes) > 256 ||
		!protectedVolume(volumes, uid, "private-mail", "", d.source.volume, d.source.claimUID) ||
		!protectedVolume(volumes, uid, d.i.namespace, p.readerUID, "data", p.claimUID) {
		return fail
	}
	return nil
}

// protectedVolume selects exactly one volume backup of the named pod volume.
// It must belong to the protection backup and the expected claim and, when
// given, the expected pod, with every byte transferred.
func protectedVolume(volumes []object, backupUID, namespace, podUID, volume, claimUID string) bool {
	var selected object
	for _, candidate := range volumes {
		spec := child(candidate, "spec")
		if text(child(spec, "pod"), "namespace") != namespace || text(spec, "volume") != volume {
			continue
		}
		if selected != nil {
			return false
		}
		selected = candidate
	}
	if selected == nil {
		return false
	}
	labels, status := child(child(selected, "metadata"), "labels"), child(selected, "status")
	progress := child(status, "progress")
	total, ok := progress["totalBytes"].(float64)
	return text(labels, "velero.io/backup-uid") == backupUID && text(labels, "velero.io/pvc-uid") == claimUID &&
		(podUID == "" || text(child(child(selected, "spec"), "pod"), "uid") == podUID) &&
		text(status, "phase") == "Completed" && ok && total > 0 && progress["bytesDone"] == total
}

// restoreDeleteReclaim returns a protected copy's volume from Retain to its
// StorageClass's Delete policy immediately before the claim is removed, so the
// backend is released with the claim. Only a protected disposal may do this.
// The patch is fenced on the observed identity, version and policy, and an
// uncertain response stops cleanup without retrying.
func (d *recoveryDriver) restoreDeleteReclaim(ctx context.Context) error {
	if d.protection.backup == "" {
		return nil
	}
	pv, found, err := d.lookup(ctx, "PersistentVolume", d.volumeName, "")
	m := child(pv, "metadata")
	if err != nil || !found || d.volumeName == "" || text(m, "uid") != d.pvUID || text(m, "name") != d.volumeName {
		return errors.New("cleanup-volume-identity")
	}
	switch text(child(pv, "spec"), "persistentVolumeReclaimPolicy") {
	case "Delete":
		return nil
	case "Retain":
	default:
		return errors.New("cleanup-volume-policy")
	}
	payload, err := json.Marshal([]object{
		{"op": "test", "path": "/metadata/uid", "value": d.pvUID},
		{"op": "test", "path": "/metadata/name", "value": d.volumeName},
		{"op": "test", "path": "/metadata/resourceVersion", "value": text(m, "resourceVersion")},
		{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"},
	})
	if err != nil || text(m, "resourceVersion") == "" {
		return errors.New("cleanup-volume-patch")
	}
	if _, err = d.k.call(ctx, payload, "patch", "PersistentVolume", d.volumeName, "--type=json", "--patch-file=/dev/stdin", "-o=json"); err != nil {
		d.uncertain = true
		return errors.New("cleanup-volume-uncertain")
	}
	after, found, err := d.lookup(ctx, "PersistentVolume", d.volumeName, "")
	if err != nil || !found || text(child(after, "metadata"), "uid") != d.pvUID || text(child(after, "spec"), "persistentVolumeReclaimPolicy") != "Delete" {
		d.uncertain = true
		return errors.New("cleanup-volume-policy")
	}
	return nil
}
