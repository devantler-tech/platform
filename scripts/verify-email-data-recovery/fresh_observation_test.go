package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type freshGraphAPI map[string]object

func (a freshGraphAPI) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	if len(input) != 0 || len(args) < 3 || args[0] != "get" || a[args[1]] == nil {
		return nil, errors.New("foreign graph access")
	}
	return json.Marshal(a[args[1]])
}

func freshGraphFixture(w *freshRecoveryWitness) freshGraphAPI {
	i := freshRetainedRecovery
	backup, snapshot, source, _ := backupProofFixture()
	child(backup, "metadata")["name"] = i.namespace + "-backup"
	child(backup, "metadata")["uid"] = w.Backup.UID
	child(backup, "metadata")["creationTimestamp"] = w.Backup.Created
	child(child(backup, "metadata"), "labels")[recoveryLabel] = i.run
	backup["spec"] = child(backupManifest(i), "spec")
	child(backup, "status")["expiration"] = w.Backup.Expires
	child(backup, "status")["completionTimestamp"] = "2026-11-10T12:45:40Z"
	owner := items(child(snapshot, "metadata"), "ownerReferences")[0]
	owner["name"], owner["uid"] = i.namespace+"-backup", w.Backup.UID
	child(snapshot, "metadata")["labels"] = object{"velero.io/pvc-uid": source.claimUID}
	child(snapshot, "status")["completionTimestamp"] = "2026-11-10T12:45:30Z"
	download := downloadManifest(i, text(child(snapshot, "status"), "snapshotID"))
	child(download, "metadata")["uid"] = w.Download.UID
	download["status"] = object{"phase": "Completed", "progress": object{"bytesDone": 4000, "totalBytes": 4000}}
	claim := recoveryManifest(i, "PersistentVolumeClaim", "data", i.namespace, "v1")
	child(claim, "metadata")["uid"] = w.Claim.UID
	claim["spec"] = object{"storageClassName": i.namespace, "volumeName": w.Claim.Volume, "resources": object{"requests": object{"storage": "2Gi"}}}
	claim["status"] = object{"phase": "Bound", "capacity": object{"storage": "2Gi"}}
	pv := object{"metadata": object{"name": w.Claim.Volume, "uid": "scratch-pv-uid"}, "spec": object{
		"claimRef":                      object{"namespace": i.namespace, "name": "data", "uid": w.Claim.UID},
		"persistentVolumeReclaimPolicy": "Delete",
		"csi":                           object{"driver": "driver.longhorn.io", "volumeHandle": w.Claim.Volume},
	}}
	volume := object{"metadata": object{"name": w.Claim.Volume, "uid": "scratch-backend-uid"}, "spec": object{"encrypted": true, "numberOfReplicas": 3, "dataEngine": "v1"}}
	return freshGraphAPI{
		"backups.velero.io":          backup,
		"podvolumebackups.velero.io": {"items": []any{snapshot}},
		"datadownloads.velero.io":    download,
		"PersistentVolumeClaim":      claim,
		"PersistentVolume":           pv,
		"volumes.longhorn.io":        volume,
	}
}

func TestFreshGraphBindsTheUnexpiredBackupToSeparateEncryptedScratch(t *testing.T) {
	w, err := parseFreshRecoveryWitness(freshFixtureWitness)
	if err != nil {
		t.Fatal("invalid test witness")
	}
	i := freshRetainedRecovery
	at := time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)
	_, _, source, _ := backupProofFixture()
	for _, tc := range []struct {
		name, stage string
		change      func(freshGraphAPI, *time.Time)
	}{
		{"matching", "", func(freshGraphAPI, *time.Time) {}},
		{"expired", "fresh-backup", func(_ freshGraphAPI, at *time.Time) { *at = at.Add(24 * time.Hour) }},
		{"backup replaced", "fresh-backup", func(a freshGraphAPI, _ *time.Time) {
			child(a["backups.velero.io"], "metadata")["uid"] = "foreign"
		}},
		{"transfer points to another snapshot", "fresh-transfer", func(a freshGraphAPI, _ *time.Time) {
			child(a["datadownloads.velero.io"], "spec")["snapshotID"] = "fedcba9876543210fedcba9876543210"
		}},
		{"snapshot points to another source claim", "fresh-transfer", func(a freshGraphAPI, _ *time.Time) {
			snapshot := items(a["podvolumebackups.velero.io"], "items")[0]
			child(child(snapshot, "metadata"), "labels")["velero.io/pvc-uid"] = "other-claim"
		}},
		{"scratch size differs", "fresh-storage", func(a freshGraphAPI, _ *time.Time) {
			child(child(a["PersistentVolumeClaim"], "status"), "capacity")["storage"] = "3Gi"
		}},
		{"scratch is plaintext", "fresh-storage", func(a freshGraphAPI, _ *time.Time) {
			child(a["volumes.longhorn.io"], "spec")["encrypted"] = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, observedAt := freshGraphFixture(w), at
			tc.change(api, &observedAt)
			claim := ownedReference{"PersistentVolumeClaim", "data", i.namespace, w.Claim.UID}
			reads := observationReadOnly{base: api, parity: true, fresh: w}
			d := &recoveryDriver{i: i, k: reads, source: source, refs: map[string]ownedReference{claim.key(): claim}}
			if stage := observeFreshRecoveryGraph(context.Background(), d, w, observedAt); stage != tc.stage {
				t.Fatalf("expected stage %q, got %q", tc.stage, stage)
			}
		})
	}
}

func TestFreshDisposalRequiresLiveNamespaceAndTimeToFinish(t *testing.T) {
	w, err := parseFreshRecoveryWitness(freshFixtureWitness)
	if err != nil {
		t.Fatal("invalid test witness")
	}
	i := freshRetainedRecovery
	_, _, source, _ := backupProofFixture()
	beforeExpiry := time.Date(2026, 11, 11, 12, 43, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, stage    string
		replace, drift bool
		horizon        time.Time
	}{
		{name: "exact graph", horizon: beforeExpiry},
		{name: "backup expires during cleanup", stage: "fresh-backup", horizon: beforeExpiry.Add(10 * time.Minute)},
		{name: "live namespace replaced", stage: "fresh-namespace", replace: true, horizon: beforeExpiry},
		{name: "frozen backup identity differs", stage: "fresh-witness", drift: true, horizon: beforeExpiry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := freshGraphFixture(w)
			ns := recoveryManifest(i, "Namespace", i.namespace, "", "v1")
			child(ns, "metadata")["uid"] = w.Namespace.UID
			if tc.replace {
				child(ns, "metadata")["uid"] = "replacement"
			}
			api["Namespace"] = ns
			nsRef := ownedReference{"Namespace", i.namespace, "", w.Namespace.UID}
			claim := ownedReference{"PersistentVolumeClaim", "data", i.namespace, w.Claim.UID}
			backup := ownedReference{"Backup", i.namespace + "-backup", "velero", w.Backup.UID}
			if tc.drift {
				backup.uid = "replacement"
			}
			refs := map[string]ownedReference{nsRef.key(): nsRef, claim.key(): claim, backup.key(): backup}
			d := &recoveryDriver{i: i, k: observationReadOnly{base: api, parity: true, fresh: w}, source: source, refs: refs}
			if stage := verifyFreshDisposalGraph(context.Background(), d, w, tc.horizon); stage != tc.stage || d.refs[backup.key()] != backup {
				t.Fatalf("expected stable backup binding and stage %q, got %q", tc.stage, stage)
			}
		})
	}
}
