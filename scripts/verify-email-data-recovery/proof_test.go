package main

import (
	"encoding/json"
	"testing"
	"time"
)

func backupProofFixture() (object, object, sourceIdentity, invocation) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	source := sourceIdentity{podUID: "source-pod-uid", claimUID: "source-claim-uid", volume: "data", pv: "source-volume", claimSize: "2Gi"}
	backup := object{"metadata": object{"name": i.namespace + "-backup", "namespace": "velero", "uid": "owned-backup-uid", "labels": object{recoveryLabel: i.run}}, "status": object{"phase": "Completed", "completionTimestamp": "2026-10-29T10:01:00Z", "errors": float64(0)}}
	pvb := object{"metadata": object{"namespace": "velero", "uid": "volume-backup-uid", "ownerReferences": []any{object{"apiVersion": "velero.io/v1", "kind": "Backup", "name": i.namespace + "-backup", "uid": "owned-backup-uid", "controller": true}}}, "spec": object{"pod": object{"name": "private-mail-0", "namespace": "private-mail", "uid": "source-pod-uid"}, "volume": "data", "backupStorageLocation": "default", "uploaderType": "kopia"}, "status": object{"phase": "Completed", "snapshotID": "0123456789abcdef0123456789abcdef", "completionTimestamp": "2026-10-29T10:00:59Z", "progress": object{"totalBytes": float64(4000), "bytesDone": float64(4000)}}}
	return backup, pvb, source, i
}

func TestCompletedSnapshotIsBoundToThisBackupAndServingVolume(t *testing.T) {
	b, p, s, i := backupProofFixture()
	snap, err := proveBackup(b, p, s, i, "owned-backup-uid", time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC))
	if err != nil || snap != "0123456789abcdef0123456789abcdef" {
		t.Fatal("completed, source-bound snapshot was refused")
	}
}

// Removing any identity, completion or byte-accounting fence would accept one
// of these foreign, stale, incomplete or empty backups as recovery evidence.
func TestUnboundOrIncompleteBackupCannotBecomeRestoreInput(t *testing.T) {
	mutations := map[string]func(object, object){
		"foreign-backup":  func(b, p object) { child(b, "metadata")["uid"] = "replacement" },
		"wrong-owner":     func(b, p object) { items(child(p, "metadata"), "ownerReferences")[0]["uid"] = "another-backup" },
		"wrong-pod":       func(b, p object) { child(child(p, "spec"), "pod")["uid"] = "replacement-pod" },
		"wrong-namespace": func(b, p object) { child(child(p, "spec"), "pod")["namespace"] = "other" },
		"wrong-volume":    func(b, p object) { child(p, "spec")["volume"] = "another" },
		"wrong-location":  func(b, p object) { child(p, "spec")["backupStorageLocation"] = "foreign" },
		"incomplete":      func(b, p object) { child(p, "status")["phase"] = "InProgress" },
		"partial":         func(b, p object) { child(child(p, "status"), "progress")["bytesDone"] = float64(3999) },
		"empty": func(b, p object) {
			child(child(p, "status"), "progress")["bytesDone"] = float64(0)
			child(child(p, "status"), "progress")["totalBytes"] = float64(0)
		},
		"invalid-snapshot": func(b, p object) { child(p, "status")["snapshotID"] = "../foreign" },
		"stale":            func(b, p object) { child(p, "status")["completionTimestamp"] = "2026-10-28T10:00:00Z" },
		"backup-errors":    func(b, p object) { child(b, "status")["errors"] = float64(1) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b, p, s, i := backupProofFixture()
			mutate(b, p)
			if _, err := proveBackup(b, p, s, i, "owned-backup-uid", time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("unsafe backup was accepted")
			}
		})
	}
}

func TestRecoveryMessageProofRequiresExactContentAndIdentity(t *testing.T) {
	message := object{"id": "owned-email", "blobId": "owned-blob", "messageId": []any{"restore-proof@example.invalid"}, "mailboxIds": object{"owned-box": true}, "textBody": []any{object{"partId": "1"}}, "bodyValues": object{"1": object{"value": "unique recovery marker\r\n", "isTruncated": false}}}
	expected := messageIdentity{email: "owned-email", blob: "owned-blob", mailbox: "owned-box", messageID: "restore-proof@example.invalid", body: "unique recovery marker"}
	if !proveMessage(message, expected) {
		t.Fatal("real recovered message was refused")
	}
	for _, key := range []string{"id", "blobId", "messageId", "mailboxIds", "bodyValues", "textBody"} {
		data, _ := json.Marshal(message)
		var changed object
		_ = json.Unmarshal(data, &changed)
		delete(changed, key)
		if proveMessage(changed, expected) {
			t.Fatal("missing message identity or content was accepted")
		}
	}
	child(child(message, "bodyValues"), "1")["isTruncated"] = true
	if proveMessage(message, expected) {
		t.Fatal("truncated recovery evidence was accepted")
	}
}

func TestNativeSuccessfulBackupOmitsZeroErrorCount(t *testing.T) {
	b, p, s, i := backupProofFixture()
	delete(child(b, "status"), "errors")
	if _, err := proveBackup(b, p, s, i, "owned-backup-uid", time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal("native omitted zero-error count was refused")
	}
	child(b, "status")["errors"] = "0"
	if _, err := proveBackup(b, p, s, i, "owned-backup-uid", time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("malformed error count was accepted")
	}
}
