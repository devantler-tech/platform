package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// protectedAPI serves the protection backup's volume list and the fenced
// reclaim patch over the cleanup fixture; every other call is the fixture's.
type protectedAPI struct {
	*cleanupAPI
	volumes  []any
	patches  []string
	patchErr bool
	// raced changes the volume between the cleanup read and its patch.
	raced bool
}

func (a *protectedAPI) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if len(args) == 5 && args[0] == "get" && args[1] == "podvolumebackups.velero.io" && args[2] == "--namespace=velero" && args[3] == "-o=json" && strings.HasPrefix(args[4], "--selector=velero.io/backup-uid=") {
		return json.Marshal(object{"metadata": object{}, "items": a.volumes})
	}
	if len(args) > 0 && args[0] == "patch" {
		a.patches = append(a.patches, strings.Join(args, " "))
		if len(args) != 6 || args[1] != "PersistentVolume" || args[3] != "--type=json" || args[4] != "--patch-file=/dev/stdin" || args[5] != "-o=json" {
			return nil, errors.New("foreign-patch")
		}
		pv, found := a.objects[cleanupKey("PersistentVolume", "", args[2])]
		var ops []object
		if !found || a.patchErr || json.Unmarshal(input, &ops) != nil {
			return nil, errors.New("patch-failed")
		}
		if a.raced {
			child(pv, "metadata")["resourceVersion"] = "8"
		}
		for _, op := range ops {
			path := strings.Split(strings.TrimPrefix(text(op, "path"), "/"), "/")
			if len(path) != 2 {
				return nil, errors.New("patch-path")
			}
			switch text(op, "op") {
			case "test":
				if !reflect.DeepEqual(child(pv, path[0])[path[1]], op["value"]) {
					return nil, errors.New("patch-test-failed")
				}
			case "replace":
				child(pv, path[0])[path[1]] = op["value"]
			default:
				return nil, errors.New("patch-op")
			}
		}
		return json.Marshal(pv)
	}
	return a.cleanupAPI.call(ctx, input, args...)
}

func protectedVolumeFixture(backupUID, namespace, pod, podUID, claimUID string) object {
	return object{
		"metadata": object{"labels": object{"velero.io/backup-name": disposalProtectionBackup, "velero.io/backup-uid": backupUID, "velero.io/pvc-uid": claimUID}},
		"spec":     object{"pod": object{"kind": "Pod", "name": pod, "namespace": namespace, "uid": podUID}, "volume": "data", "uploaderType": "kopia"},
		"status":   object{"phase": "Completed", "snapshotID": "0123456789abcdef0123456789abcdef", "progress": object{"bytesDone": float64(4000), "totalBytes": float64(4000)}},
	}
}

// protectedCleanupFixture is the cleanup fixture after Velero collected the
// copy's own backup: disposal is bound to the fixed protection backup, and the
// copy's volume carries the Retain policy that protected it in the meantime.
func protectedCleanupFixture(t *testing.T) (*recoveryDriver, *protectedAPI, context.Context) {
	t.Helper()
	d, base, ctx := cleanupFixture(t, false)
	trial := cleanupKey("Backup", "velero", d.i.namespace+"-backup")
	delete(base.objects, trial)
	delete(d.refs, trial)
	readerUID := text(child(base.objects[cleanupKey("Pod", d.i.namespace, "reader")], "metadata"), "uid")
	claimUID := text(child(base.objects[cleanupKey("PersistentVolumeClaim", d.i.namespace, "data")], "metadata"), "uid")
	d.protection = protectionPoint{backup: disposalProtectionBackup, readerUID: readerUID, claimUID: claimUID}
	base.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)] = object{
		"kind":     "Backup",
		"metadata": object{"name": disposalProtectionBackup, "namespace": "velero", "uid": "protection-uid"},
		"status":   object{"phase": "PartiallyFailed", "expiration": time.Now().Add(13 * 24 * time.Hour).UTC().Format(time.RFC3339)},
	}
	pv := base.objects[cleanupKey("PersistentVolume", "", d.volumeName)]
	child(pv, "metadata")["resourceVersion"] = "7"
	child(pv, "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	a := &protectedAPI{cleanupAPI: base, volumes: []any{
		protectedVolumeFixture("protection-uid", "private-mail", "private-mail-0", "serving-pod-uid", d.source.claimUID),
		protectedVolumeFixture("protection-uid", d.i.namespace, "reader", readerUID, claimUID),
	}}
	d.k = a
	return d, a, ctx
}

func TestProtectionRequiresTheFixedBackupWithBothCompletedVolumes(t *testing.T) {
	horizon := time.Now().Add(10 * time.Minute)
	for _, tc := range []struct {
		name   string
		accept bool
		change func(*recoveryDriver, *protectedAPI)
	}{
		{"partially failed daily backup with both volumes", true, func(*recoveryDriver, *protectedAPI) {}},
		{"completed daily backup", true, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "status")["phase"] = "Completed"
		}},
		{"another protection point", false, func(d *recoveryDriver, _ *protectedAPI) { d.protection.backup = "velero-daily-full-20261113021735" }},
		{"no witnessed reader", false, func(d *recoveryDriver, _ *protectedAPI) { d.protection.readerUID = "" }},
		{"backup collected", false, func(_ *recoveryDriver, a *protectedAPI) {
			delete(a.objects, cleanupKey("Backup", "velero", disposalProtectionBackup))
		}},
		{"backup being deleted", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "metadata")["deletionTimestamp"] = "2026-11-13T00:00:00Z"
		}},
		{"backup expires before cleanup ends", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "status")["expiration"] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
		}},
		{"backup failed", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "status")["phase"] = "Failed"
		}},
		{"serving mailbox missing", false, func(_ *recoveryDriver, a *protectedAPI) { a.volumes = a.volumes[1:] }},
		{"copy missing", false, func(_ *recoveryDriver, a *protectedAPI) { a.volumes = a.volumes[:1] }},
		{"copy duplicated", false, func(_ *recoveryDriver, a *protectedAPI) { a.volumes = append(a.volumes, a.volumes[1]) }},
		{"copy from another reader", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(child(a.volumes[1].(object), "spec"), "pod")["uid"] = "replacement"
		}},
		{"copy of another claim", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(child(a.volumes[1].(object), "metadata"), "labels")["velero.io/pvc-uid"] = "replacement"
		}},
		{"serving mailbox of another claim", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(child(a.volumes[0].(object), "metadata"), "labels")["velero.io/pvc-uid"] = "replacement"
		}},
		{"volume of another backup", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(child(a.volumes[1].(object), "metadata"), "labels")["velero.io/backup-uid"] = "other-backup"
		}},
		{"copy not completed", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.volumes[1].(object), "status")["phase"] = "Canceled"
		}},
		{"copy partly transferred", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(child(a.volumes[1].(object), "status"), "progress")["bytesDone"] = float64(3999)
		}},
		{"malformed volume entry", false, func(_ *recoveryDriver, a *protectedAPI) { a.volumes = append(a.volumes, "foreign") }},
		{"copy without snapshot", false, func(_ *recoveryDriver, a *protectedAPI) {
			delete(child(a.volumes[1].(object), "status"), "snapshotID")
		}},
		{"copy being deleted", false, func(_ *recoveryDriver, a *protectedAPI) {
			child(a.volumes[1].(object), "metadata")["deletionTimestamp"] = "2026-11-13T00:00:00Z"
		}},
		{"backup replaced under the same name", false, func(d *recoveryDriver, _ *protectedAPI) { d.protection.backupUID = "earlier-observation" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, a, ctx := protectedCleanupFixture(t)
			tc.change(d, a)
			if err := d.verifyProtection(ctx, horizon); (err == nil) != tc.accept {
				t.Fatalf("protection result mismatch: %v", err)
			}
		})
	}
}

// The protected copy's volume returns to Delete under a fenced patch after the
// final verification and before the first removal, so its claim releases it.
func TestProtectedCleanupReleasesARetainedVolumeWithItsClaim(t *testing.T) {
	d, a, ctx := protectedCleanupFixture(t)
	pvKey := cleanupKey("PersistentVolume", "", d.volumeName)
	policyAtReaderRemoval, policyAtClaimRemoval := "", ""
	a.onDelete = func(o object) {
		switch text(o, "kind") {
		case "Pod":
			policyAtReaderRemoval = text(child(a.objects[pvKey], "spec"), "persistentVolumeReclaimPolicy")
		case "PersistentVolumeClaim":
			policyAtClaimRemoval = text(child(a.objects[pvKey], "spec"), "persistentVolumeReclaimPolicy")
		case "Backup":
			t.Error("cleanup removed a backup")
		}
	}
	verified := false
	err := d.cleanupRestoreAfterVerification(ctx, func(context.Context) error {
		if len(a.patches) != 0 {
			t.Fatal("volume policy changed before final verification")
		}
		verified = true
		return nil
	})
	if err != nil || !verified {
		t.Fatalf("protected cleanup failed: %v", err)
	}
	if policyAtReaderRemoval != "Delete" || policyAtClaimRemoval != "Delete" || len(a.patches) != 1 {
		t.Fatalf("reader removed with policy %q and claim with %q after %d patches", policyAtReaderRemoval, policyAtClaimRemoval, len(a.patches))
	}
	for key := range a.objects {
		if strings.Contains(key, "/"+d.i.namespace+"/") || key == pvKey {
			t.Fatalf("protected cleanup left %s", key)
		}
	}
	if _, kept := a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)]; !kept || len(d.refs) != 0 {
		t.Fatal("protection backup or owned residue changed")
	}
}

// Every refusal before the first removal keeps the whole copy, reader
// included, so a later confirmed attempt can still adopt it.
func TestProtectedCleanupRefusesWithoutProtectionOrCertainPolicy(t *testing.T) {
	for _, tc := range []struct{ failure, err string }{
		{"protection expires during cleanup", "cleanup-backup-retention"},
		{"protection collected", "cleanup-backup-retention"},
		{"uncertain patch", "cleanup-volume-uncertain"},
		{"changed volume version", "cleanup-volume-uncertain"},
		{"policy changed after verification", "cleanup-volume-policy"},
		{"volume replaced after verification", "cleanup-volume-identity"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			d, a, ctx := protectedCleanupFixture(t)
			pv := a.objects[cleanupKey("PersistentVolume", "", d.volumeName)]
			switch tc.failure {
			case "protection expires during cleanup":
				child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "status")["expiration"] = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
			case "protection collected":
				delete(a.objects, cleanupKey("Backup", "velero", disposalProtectionBackup))
			case "uncertain patch":
				a.patchErr = true
			case "changed volume version":
				a.raced = true
			}
			err := d.cleanupRestoreAfterVerification(ctx, func(context.Context) error {
				switch tc.failure {
				case "policy changed after verification":
					child(pv, "spec")["persistentVolumeReclaimPolicy"] = "Recycle"
				case "volume replaced after verification":
					child(pv, "metadata")["uid"] = "replacement"
				}
				return nil
			})
			if err == nil || err.Error() != tc.err {
				t.Fatalf("expected refusal %q, got %v", tc.err, err)
			}
			for _, key := range []string{cleanupKey("Pod", d.i.namespace, "reader"), cleanupKey("PersistentVolumeClaim", d.i.namespace, "data"), cleanupKey("Secret", d.i.namespace, "storage-key"), cleanupKey("PersistentVolume", "", d.volumeName)} {
				if _, present := a.objects[key]; !present {
					t.Fatalf("refused cleanup removed %s", key)
				}
			}
		})
	}
}

// adoptableGraph gives the cleanup fixture the live shapes graph adoption
// checks: encoded secrets, a running reader and a finished transfer.
func adoptableGraph(d *recoveryDriver, a *protectedAPI) {
	for _, o := range a.objects {
		if text(o, "kind") == "Secret" {
			data := object{}
			for k, v := range child(o, "stringData") {
				data[k] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
			}
			o["data"] = data
			delete(o, "stringData")
		}
	}
	child(a.objects[cleanupKey("Secret", d.i.namespace, "storage-key")], "data")["CRYPTO_KEY_VALUE"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 64)))
	child(a.objects[cleanupKey("Secret", d.i.namespace, "test-authentication")], "data")["credential"] = base64.StdEncoding.EncodeToString([]byte("restore:" + strings.Repeat("b", 64)))
	a.objects[cleanupKey("Pod", d.i.namespace, "reader")]["status"] = object{"phase": "Running"}
	download := a.objects[cleanupKey("DataDownload", "velero", d.i.namespace+"-download")]
	child(download, "status")["progress"] = object{"totalBytes": float64(100), "bytesDone": float64(100)}
}

// Only a protected disposal may adopt a graph whose own backup is gone, and
// only it accepts a Retain policy on the copy's volume.
func TestOnlyProtectedDisposalAcceptsTheCollectedBackupAndRetainedVolume(t *testing.T) {
	trialBackup := cleanupKey("Backup", "velero", "")
	for _, tc := range []struct {
		name               string
		protected, restore bool
	}{
		{"protected without its own backup", true, false},
		{"unprotected without its own backup", false, false},
		{"unprotected with its own backup", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, a, ctx := protectedCleanupFixture(t)
			trialBackup = cleanupKey("Backup", "velero", d.i.namespace+"-backup")
			reader, claim := d.protection.readerUID, d.protection.claimUID
			if !tc.protected {
				d.protection = protectionPoint{}
			}
			if err := d.observeRestoredStorage(ctx); (err == nil) != tc.protected {
				t.Fatalf("retained volume result mismatch: %v", err)
			}
			adoptableGraph(d, a)
			if tc.restore {
				backup := backupManifest(d.i)
				child(backup, "metadata")["uid"] = "owned-Backup-" + d.i.namespace + "-backup"
				a.objects[trialBackup] = backup
			}
			d.refs = map[string]ownedReference{}
			err := d.adoptRetainedGraph(ctx, reader, claim)
			if accepted := tc.protected || tc.restore; (err == nil) != accepted {
				t.Fatalf("graph adoption mismatch: %v", err)
			}
			if _, tracked := d.refs[trialBackup]; err == nil && tracked != tc.restore {
				t.Fatal("graph adoption tracked the wrong backup")
			}
			if want := map[bool]int{true: 9, false: 10}[tc.protected]; err == nil && len(d.refs) != want {
				t.Fatalf("adopted %d objects, want %d", len(d.refs), want)
			}
		})
	}
}

// A protected fresh disposal no longer reads the copy's collected backup; the
// fixed protection backup must instead outlast the cleanup budget.
func TestProtectedFreshGraphBindsTheCopyToTheProtectionBackup(t *testing.T) {
	w, err := parseFreshRecoveryWitness(freshFixtureWitness)
	if err != nil {
		t.Fatal("invalid test witness")
	}
	i := freshRetainedRecovery
	_, _, source, _ := backupProofFixture()
	horizon := time.Now().Add(10 * time.Minute)
	for _, tc := range []struct {
		name, stage string
		change      func(freshGraphAPI)
	}{
		{"matching", "", func(freshGraphAPI) {}},
		{"protection expires during cleanup", "fresh-protection", func(a freshGraphAPI) {
			child(a["backups.velero.io"], "status")["expiration"] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
		}},
		{"copy absent from protection", "fresh-protection", func(a freshGraphAPI) {
			a["podvolumebackups.velero.io"]["items"] = items(a["podvolumebackups.velero.io"], "items")[:1]
		}},
		{"scratch is plaintext", "fresh-storage", func(a freshGraphAPI) {
			child(a["volumes.longhorn.io"], "spec")["encrypted"] = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := freshGraphFixture(w)
			api["backups.velero.io"] = object{"metadata": object{"name": disposalProtectionBackup, "namespace": "velero", "uid": "protection-uid"}, "status": object{"phase": "PartiallyFailed", "expiration": time.Now().Add(13 * 24 * time.Hour).UTC().Format(time.RFC3339)}}
			api["podvolumebackups.velero.io"] = object{"items": []any{
				protectedVolumeFixture("protection-uid", "private-mail", "private-mail-0", source.podUID, source.claimUID),
				protectedVolumeFixture("protection-uid", i.namespace, "reader", w.Reader.UID, w.Claim.UID),
			}}
			ns := recoveryManifest(i, "Namespace", i.namespace, "", "v1")
			child(ns, "metadata")["uid"] = w.Namespace.UID
			api["Namespace"] = ns
			tc.change(api)
			nsRef := ownedReference{"Namespace", i.namespace, "", w.Namespace.UID}
			claim := ownedReference{"PersistentVolumeClaim", "data", i.namespace, w.Claim.UID}
			d := &recoveryDriver{i: i, k: api, source: source, refs: map[string]ownedReference{nsRef.key(): nsRef, claim.key(): claim}, protection: protectionPoint{backup: disposalProtectionBackup, readerUID: w.Reader.UID, claimUID: w.Claim.UID}}
			if stage := verifyFreshDisposalGraph(context.Background(), d, w, horizon); stage != tc.stage {
				t.Fatalf("expected stage %q, got %q", tc.stage, stage)
			}
			if _, tracked := d.refs[cleanupKey("Backup", "velero", i.namespace+"-backup")]; tracked {
				t.Fatal("protected graph tracked the collected backup")
			}
		})
	}
}

// Cleanup cannot certify a removal whose protection point vanished meanwhile.
func TestProtectedCleanupReportsAProtectionLostDuringRemoval(t *testing.T) {
	d, a, ctx := protectedCleanupFixture(t)
	a.onDelete = func(o object) {
		if text(o, "kind") == "Namespace" {
			delete(a.objects, cleanupKey("Backup", "velero", disposalProtectionBackup))
		}
	}
	if err := d.cleanupRestore(ctx); err == nil || err.Error() != "backup-retention" {
		t.Fatalf("lost protection was certified: %v", err)
	}
}

// The first successful observation pins the protection backup's UID.
func TestProtectionPinsTheFirstObservedBackup(t *testing.T) {
	d, a, ctx := protectedCleanupFixture(t)
	horizon := time.Now().Add(10 * time.Minute)
	if err := d.verifyProtection(ctx, horizon); err != nil || d.protection.backupUID != "protection-uid" {
		t.Fatalf("first observation was not pinned: %v", err)
	}
	child(a.objects[cleanupKey("Backup", "velero", disposalProtectionBackup)], "metadata")["uid"] = "replacement"
	for _, v := range a.volumes {
		child(child(v.(object), "metadata"), "labels")["velero.io/backup-uid"] = "replacement"
	}
	if d.verifyProtection(ctx, horizon) == nil {
		t.Fatal("a replacement of the same name was accepted")
	}
}
