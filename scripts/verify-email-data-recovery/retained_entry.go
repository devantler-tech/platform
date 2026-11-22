package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Bind the retained transfer to the controller-owned volume snapshot. The
// creation time is only a controller chronology check, never marker freshness.
func (d *recoveryDriver) retainedSnapshot(ctx context.Context, sourceClaimUID string) error {
	backup, err := d.ref("Backup", d.i.namespace+"-backup", "velero")
	if err != nil {
		return err
	}
	b, err := d.owned(ctx, backup)
	if err != nil {
		return err
	}
	data, err := d.k.call(ctx, nil, "get", "podvolumebackups.velero.io", "--namespace=velero", "-o=json", "--selector=velero.io/backup-uid="+backup.uid)
	if err != nil {
		return errors.New("retained-snapshot-list")
	}
	var list object
	if json.Unmarshal(data, &list) != nil || text(child(list, "metadata"), "continue") != "" || child(list, "metadata")["remainingItemCount"] != nil {
		return errors.New("retained-snapshot-list")
	}
	raw, ok := list["items"].([]any)
	volumes := items(list, "items")
	if !ok || len(raw) != len(volumes) || len(volumes) > 256 {
		return errors.New("retained-snapshot-list")
	}
	volume, err := sourceVolumeBackup(volumes, d.source.volume)
	if err != nil {
		return err
	}
	if sourceClaimUID != "" && text(child(child(volume, "metadata"), "labels"), "velero.io/pvc-uid") != sourceClaimUID {
		return errors.New("retained-snapshot-source-claim")
	}
	sourceAtBackup := d.source
	sourceAtBackup.podUID = text(child(child(volume, "spec"), "pod"), "uid")
	created, err := time.Parse(time.RFC3339, text(child(b, "metadata"), "creationTimestamp"))
	if err != nil {
		return errors.New("retained-snapshot-time")
	}
	snapshot, err := proveBackup(b, volume, sourceAtBackup, d.i, backup.uid, created)
	if err != nil {
		return err
	}
	transfer, err := d.ref("DataDownload", d.i.namespace+"-download", "velero")
	if err != nil {
		return err
	}
	live, err := d.owned(ctx, transfer)
	if err != nil || text(child(live, "spec"), "snapshotID") != snapshot {
		return errors.New("retained-snapshot-transfer")
	}
	return nil
}
