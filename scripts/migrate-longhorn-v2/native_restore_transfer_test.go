package main

import "testing"

func nativeRestoreTransferFixture(t *testing.T) (benchmarkRecord, canarySeedState, nativeRestoreState, object, object, object, object) {
	t.Helper()
	record, seed, state := nativeRestoreStateFixture(t, "restore-owned")
	targets := map[string]string{}
	for _, target := range state.Targets {
		targets[target.Name] = target.UID
	}
	assets, err := nativeRestoreAssets(record, seed, state.Run, targets)
	if err != nil {
		t.Fatal(err)
	}
	var restore object
	for _, asset := range assets {
		if text(asset, "kind") == "Restore" {
			restore = asset
		}
	}
	restore["metadata"].(object)["uid"] = state.Resources["Restore"].UID
	restore["metadata"].(object)["resourceVersion"] = "14"
	restore["status"] = object{"phase": "Completed", "completionTimestamp": "2026-11-15T10:01:00Z"}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": "fio", "namespace": benchmarkNamespace, "uid": "fresh-restore-pod", "resourceVersion": "22", "labels": object{"storage.platform.devantler.tech/benchmark-run": state.Run, "velero.io/restore-name": state.Resources["Restore"].Name}},
		"spec":   object{"nodeName": state.Targets[0].Name, "volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "fio-data"}}, object{"name": "scratch", "emptyDir": object{}}}},
		"status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}}
	_, _, transfer := transferFixture("Restore")
	transfer["metadata"] = object{"name": "native-restore-transfer", "namespace": "velero", "uid": "native-restore-transfer-uid", "resourceVersion": "30", "ownerReferences": []any{object{"apiVersion": "velero.io/v1", "kind": "Restore", "name": state.Resources["Restore"].Name, "uid": state.Resources["Restore"].UID, "controller": true}}}
	transfer["spec"].(object)["pod"] = object{"name": "fio", "namespace": benchmarkNamespace, "uid": text(pod, "metadata", "uid")}
	transfer["spec"].(object)["snapshotID"] = seed.Snapshot
	transfer["spec"].(object)["sourceNamespace"] = benchmarkNamespace
	repository := canaryRepositoryFixture()
	repository["metadata"].(object)["name"] = seed.Repository.Name
	repository["metadata"].(object)["uid"] = seed.Repository.UID
	valueObject(repository, "metadata", "labels")["velero.io/volume-namespace"] = benchmarkNamespace
	repository["spec"].(object)["volumeNamespace"] = benchmarkNamespace
	seed.RepositorySpec, err = seedSpecDigest(repository)
	if err != nil {
		t.Fatal(err)
	}
	state.SeedDigest, err = nativeRestoreSeedDigest(record, seed)
	if err != nil {
		t.Fatal(err)
	}
	return record, seed, state, restore, pod, transfer, repository
}

func TestNativeRestoreTransferBindsFreshConsumerAndOriginalKopiaSnapshot(t *testing.T) {
	for _, fault := range []string{"", "replaced restore", "wrong backup", "restores old PV", "namespace remap", "restore warnings", "wrong run", "reused source pod", "borrowed backup UID", "wrong node", "wrong claim", "extra claim", "pod terminating", "pod not ready", "wrong restore label", "wrong snapshot", "partial copy", "wrong transfer owner", "borrowed transfer UID", "aliased child UIDs", "replaced repository", "changed repository", "missing transfer", "two transfers", "missing repository", "unrecorded transfer"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, restore, pod, transfer, repository := nativeRestoreTransferFixture(t)
			transfers, repositories := []object{transfer}, []object{repository}
			switch fault {
			case "replaced restore":
				valueObject(restore, "metadata")["uid"] = "replacement"
			case "wrong backup":
				valueObject(restore, "spec")["backupName"] = "another-backup"
			case "restores old PV":
				valueObject(restore, "spec")["restorePVs"] = true
			case "namespace remap":
				valueObject(restore, "spec")["namespaceMapping"] = object{benchmarkNamespace: "another-namespace"}
			case "restore warnings":
				valueObject(restore, "status")["warnings"] = float64(1)
			case "wrong run":
				valueObject(pod, "metadata", "labels")["storage.platform.devantler.tech/benchmark-run"] = r.Run
			case "reused source pod":
				valueObject(pod, "metadata")["uid"] = r.Resources["Pod"].UID
				valueObject(transfer, "spec", "pod")["uid"] = r.Resources["Pod"].UID
			case "borrowed backup UID":
				valueObject(pod, "metadata")["uid"] = seed.Backup.UID
				valueObject(transfer, "spec", "pod")["uid"] = seed.Backup.UID
			case "wrong node":
				valueObject(pod, "spec")["nodeName"] = state.Targets[1].Name
			case "wrong claim":
				valueObject(pod, "spec")["volumes"] = []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "application-data"}}}
			case "extra claim":
				valueObject(pod, "spec")["volumes"] = append(value(pod, "spec", "volumes").([]any), object{"name": "application", "persistentVolumeClaim": object{"claimName": "application-data"}})
			case "pod terminating":
				valueObject(pod, "metadata")["deletionTimestamp"] = "2026-11-15T10:01:00Z"
			case "pod not ready":
				valueObject(pod, "status")["conditions"] = []any{}
			case "wrong restore label":
				valueObject(pod, "metadata", "labels")["velero.io/restore-name"] = "another-restore"
			case "wrong snapshot":
				valueObject(transfer, "spec")["snapshotID"] = "another-snapshot"
			case "partial copy":
				valueObject(transfer, "status", "progress")["bytesDone"] = float64(64)
			case "wrong transfer owner":
				valueObject(transfer, "metadata")["ownerReferences"] = []any{}
			case "borrowed transfer UID":
				valueObject(transfer, "metadata")["uid"] = seed.Transfer.UID
			case "aliased child UIDs":
				valueObject(transfer, "metadata")["uid"] = text(pod, "metadata", "uid")
			case "replaced repository":
				valueObject(repository, "metadata")["uid"] = "replacement-repository"
			case "changed repository":
				valueObject(repository, "spec")["resticIdentifier"] = "another-repository"
			case "missing transfer":
				transfers = []object{}
			case "two transfers":
				transfers = append(transfers, transfer)
			case "missing repository":
				repositories = []object{}
			case "unrecorded transfer":
				state.Phase = "restore-create-requested"
				delete(state.Resources, "Restore")
			}
			receipt, err := verifyNativeRestoreTransfer(r, seed, state, restore, pod, transfers, repositories)
			if (err != nil) != (fault != "") {
				t.Fatalf("restore transfer verdict for %q: %v", fault, err)
			}
			if fault == "" && (receipt.Pod != benchmarkIdentity(pod) || receipt.Transfer != benchmarkIdentity(transfer) || receipt.Node != state.Targets[0].Name) {
				t.Fatal("the original restore child receipts were not retained")
			}
		})
	}
}

func TestNativeRestoreTransferRebindsRecordedChildren(t *testing.T) {
	for _, fault := range []string{"", "consumer replaced", "transfer replaced"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, restore, pod, transfer, repository := nativeRestoreTransferFixture(t)
			state.Phase, state.FirstNode = "restore-verified", state.Targets[0].Name
			state.Resources["FirstPod"], state.Resources["PodVolumeRestore"] = benchmarkIdentity(pod), benchmarkIdentity(transfer)
			if state.validate(r, seed) != nil {
				t.Fatal("the recorded native transfer fixture is invalid")
			}
			if fault == "consumer replaced" {
				valueObject(pod, "metadata")["uid"] = "replacement-consumer"
				valueObject(transfer, "spec", "pod")["uid"] = "replacement-consumer"
			}
			if fault == "transfer replaced" {
				valueObject(transfer, "metadata")["uid"] = "replacement-transfer"
			}
			_, err := verifyNativeRestoreTransfer(r, seed, state, restore, pod, []object{transfer}, []object{repository})
			if (err != nil) != (fault != "") {
				t.Fatalf("replaced native transfer evidence was adopted: fault=%q err=%v", fault, err)
			}
		})
	}
}
