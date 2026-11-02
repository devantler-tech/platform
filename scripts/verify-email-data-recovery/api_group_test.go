package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"
)

// Unqualified Backup resolves to Longhorn on the real cluster. Model that
// independent API collision rather than accepting every kind as Velero.
type shadowedBackupAPI struct{}

func (shadowedBackupAPI) call(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	if len(args) < 2 || args[0] != "get" {
		return nil, errors.New("unexpected-command")
	}
	if args[1] != "backups.velero.io" {
		return nil, nil
	}
	backup := object{"apiVersion": "velero.io/v1", "kind": "Backup", "metadata": object{"name": "owned-backup", "namespace": "velero", "uid": "velero-backup-uid"}}
	if len(args) > 2 && args[2] == "owned-backup" {
		return json.Marshal(backup)
	}
	return json.Marshal(object{"items": []any{backup}})
}

func TestVeleroLookupAndListDoNotResolveToLonghornBackups(t *testing.T) {
	d := recoveryDriver{k: shadowedBackupAPI{}}
	t.Run("lookup", func(t *testing.T) {
		backup, found, err := d.lookup(context.Background(), "Backup", "owned-backup", "velero")
		if err != nil || !found || text(backup, "apiVersion") != "velero.io/v1" || text(child(backup, "metadata"), "uid") != "velero-backup-uid" {
			t.Fatal("the owned Velero backup was hidden by another API group")
		}
	})
	t.Run("list", func(t *testing.T) {
		backups, err := d.list(context.Background(), "Backup", "velero", "")
		if err != nil || len(backups) != 1 || text(backups[0], "apiVersion") != "velero.io/v1" {
			t.Fatal("backup discovery read the shadowing API group")
		}
	})
}

// Opt-in, fixed-scope reads exercise the actual driver through the cluster's
// native discovery collision. They create, restore and delete nothing.
func TestReadOnlyVeleroBackupAPISelection(t *testing.T) {
	contextName, name := os.Getenv("EMAIL_RECOVERY_READONLY_SOURCE_CONTEXT"), os.Getenv("EMAIL_RECOVERY_READONLY_BACKUP_NAME")
	if contextName == "" || name == "" {
		t.Skip("requires an explicit existing read-only context and retained trial backup")
	}
	if !regexp.MustCompile(`^email-data-recovery-[1-9][0-9]{6,19}-backup$`).MatchString(name) {
		t.Fatal("refused a backup outside the recovery trial namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d := recoveryDriver{k: kubectl{context: contextName}}
	backup, found, err := d.lookup(ctx, "Backup", name, "velero")
	if err != nil || !found || text(backup, "apiVersion") != "velero.io/v1" || text(backup, "kind") != "Backup" || text(child(backup, "metadata"), "uid") == "" {
		t.Fatal("native Velero backup identity was not observed")
	}
	volumes, err := d.list(ctx, "PodVolumeBackup", "velero", "velero.io/backup-uid="+text(child(backup, "metadata"), "uid"))
	if err != nil || len(volumes) == 0 {
		t.Fatal("native Velero volume receipts were not observed")
	}
	for _, volume := range volumes {
		if text(volume, "apiVersion") != "velero.io/v1" || text(volume, "kind") != "PodVolumeBackup" {
			t.Fatal("native volume read used a different resource family")
		}
	}
	t.Log("READONLY_VELERO_RESOURCE_SELECTION=VERIFIED mutations=0")
}
