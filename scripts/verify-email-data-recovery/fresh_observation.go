package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const freshParityConfirm = "verify-fresh-retained-email-parity"

var freshRetainedRecovery = invocation{
	run:       "42818236850",
	revision:  "7df29aa835f3d1e79893294e06b3f527f6dd3839",
	namespace: "email-data-recovery-42818236850",
}

type freshRecoveryWitness struct {
	Run       int64 `json:"run"`
	Namespace struct {
		UID string `json:"uid"`
	} `json:"namespace"`
	Reader struct {
		UID string `json:"uid"`
	} `json:"reader"`
	Claim struct {
		UID    string `json:"uid"`
		Volume string `json:"volume"`
	} `json:"claim"`
	Backup struct {
		UID     string `json:"uid"`
		Created string `json:"created"`
		Expires string `json:"expires"`
	} `json:"backup"`
	Download struct {
		UID string `json:"uid"`
	} `json:"download"`
}

func parseFreshRecoveryWitness(data string) (*freshRecoveryWitness, error) {
	fields := map[string][]string{
		"":          {"run", "namespace", "reader", "claim", "backup", "download"},
		"namespace": {"uid"},
		"reader":    {"uid"},
		"claim":     {"uid", "volume"},
		"backup":    {"uid", "created", "expires"},
		"download":  {"uid"},
	}
	var witness freshRecoveryWitness
	if len(data) > 16384 || strictjson.Validate([]byte(data), fields) != nil ||
		json.Unmarshal([]byte(data), &witness) != nil ||
		witness.Run != 42818236850 || !kubernetesName.MatchString(witness.Claim.Volume) {
		return nil, errors.New("fresh-witness")
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for _, uid := range []string{witness.Namespace.UID, witness.Reader.UID, witness.Claim.UID, witness.Backup.UID, witness.Download.UID} {
		if !uuid.MatchString(uid) || seen[uid] {
			return nil, errors.New("fresh-witness")
		}
		seen[uid] = true
	}
	// The protected run began its mailbox step at 12:44:23 UTC and created
	// this backup at 12:44:40 UTC. Neither window boundary comes from JMAP.
	if witness.Backup.Created != "2026-11-10T12:44:40Z" || witness.Backup.Expires != "2026-11-11T12:44:40Z" {
		return nil, errors.New("fresh-witness")
	}
	return &witness, nil
}

func (w *freshRecoveryWitness) reads() [][]string {
	ns := freshRetainedRecovery.namespace
	return [][]string{
		{"get", "backups.velero.io", ns + "-backup", "--ignore-not-found=true", "-o=json", "--namespace=velero"},
		{"get", "datadownloads.velero.io", ns + "-download", "--ignore-not-found=true", "-o=json", "--namespace=velero"},
		{"get", "podvolumebackups.velero.io", "--namespace=velero", "-o=json", "--selector=velero.io/backup-uid=" + w.Backup.UID},
		{"get", "PersistentVolume", w.Claim.Volume, "--ignore-not-found=true", "-o=json"},
		{"get", "volumes.longhorn.io", w.Claim.Volume, "--ignore-not-found=true", "-o=json", "--namespace=longhorn-system"},
	}
}

func observeFreshRecoveryGraph(ctx context.Context, d *recoveryDriver, w *freshRecoveryWitness, observedAt time.Time) string {
	backupRef := ownedReference{"Backup", d.i.namespace + "-backup", "velero", w.Backup.UID}
	downloadRef := ownedReference{"DataDownload", d.i.namespace + "-download", "velero", w.Download.UID}
	for _, ref := range []ownedReference{backupRef, downloadRef} {
		d.refs[ref.key()] = ref
	}
	backup, err := d.owned(ctx, backupRef)
	expires, parseErr := time.Parse(time.RFC3339, w.Backup.Expires)
	meta, status := child(backup, "metadata"), child(backup, "status")
	if err != nil || parseErr != nil || !observedAt.Before(expires) || meta["deletionTimestamp"] != nil ||
		text(meta, "creationTimestamp") != w.Backup.Created || text(status, "expiration") != w.Backup.Expires ||
		!retainedBackupSpec(child(backupManifest(d.i), "spec"), child(backup, "spec")) {
		return "fresh-backup"
	}
	download, err := d.owned(ctx, downloadRef)
	spec, status := child(download, "spec"), child(download, "status")
	want := child(downloadManifest(d.i, text(spec, "snapshotID")), "spec")
	if err != nil || child(download, "metadata")["deletionTimestamp"] != nil ||
		text(status, "phase") != "Completed" || !reflect.DeepEqual(child(spec, "targetVolume"), child(want, "targetVolume")) {
		return "fresh-transfer"
	}
	for _, key := range []string{"backupStorageLocation", "datamover", "sourceNamespace", "operationTimeout", "nodeOS"} {
		if spec[key] != want[key] {
			return "fresh-transfer"
		}
	}
	progress := child(status, "progress")
	total, ok := progress["totalBytes"].(float64)
	if !ok || total <= 0 || progress["bytesDone"] != total || d.source.claimUID == "" ||
		d.retainedSnapshot(ctx, d.source.claimUID) != nil {
		return "fresh-transfer"
	}
	claimRef, err := d.ref("PersistentVolumeClaim", "data", d.i.namespace)
	if err != nil {
		return "fresh-storage"
	}
	claim, err := d.owned(ctx, claimRef)
	if err != nil || child(claim, "metadata")["deletionTimestamp"] != nil ||
		text(child(child(child(claim, "spec"), "resources"), "requests"), "storage") != "2Gi" ||
		text(child(child(claim, "status"), "capacity"), "storage") != "2Gi" {
		return "fresh-storage"
	}
	if d.observeRestoredStorage(ctx) != nil || d.volumeName != w.Claim.Volume {
		return "fresh-storage"
	}
	return ""
}

func freshRecoveryWindow() *retainedMessageWindow {
	// Query from the second before the protected step. Check the returned
	// timestamp independently so that second cannot qualify as evidence.
	return &retainedMessageWindow{
		after:  time.Date(2026, 11, 10, 12, 44, 22, 0, time.UTC),
		before: time.Date(2026, 11, 10, 12, 44, 41, 0, time.UTC),
	}
}
