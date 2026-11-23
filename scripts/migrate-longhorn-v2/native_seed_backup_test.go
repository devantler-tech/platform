package main

import (
	"encoding/json"
	"testing"
)

func retainedSeedBackupFixture(t *testing.T) (benchmarkRecord, canarySeedState, object, object, object, object) {
	t.Helper()
	r, state := seedStateFixture(t, "backup-verified")
	backup, err := nativeSeedBackup(r)
	if err != nil {
		t.Fatal(err)
	}
	backup["metadata"].(object)["uid"] = state.Backup.UID
	backup["metadata"].(object)["resourceVersion"] = "21"
	backup["status"] = object{"phase": "Completed", "completionTimestamp": "2026-11-15T06:00:00Z"}
	_, pod, transfer := transferFixture("Backup")
	pod["metadata"].(object)["namespace"] = benchmarkNamespace
	pod["metadata"].(object)["uid"] = r.Resources["Pod"].UID
	transfer["metadata"].(object)["name"] = state.Transfer.Name
	transfer["metadata"].(object)["uid"] = state.Transfer.UID
	transfer["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "velero.io/v1", "kind": "Backup", "name": state.Backup.Name, "uid": state.Backup.UID, "controller": true}}
	transfer["spec"].(object)["pod"] = object{"name": "fio", "namespace": benchmarkNamespace, "uid": r.Resources["Pod"].UID}
	transfer["status"].(object)["snapshotID"] = state.Snapshot
	repository := canaryRepositoryFixture()
	repository["metadata"].(object)["name"] = state.Repository.Name
	repository["metadata"].(object)["uid"] = state.Repository.UID
	repository["metadata"].(object)["labels"].(object)["velero.io/volume-namespace"] = benchmarkNamespace
	repository["spec"].(object)["volumeNamespace"] = benchmarkNamespace
	state.RepositorySpec, err = seedSpecDigest(repository)
	if err != nil {
		t.Fatal(err)
	}
	return r, state, backup, pod, transfer, repository
}

func TestNativeSeedBackupCannotSelectApplicationDataOrSnapshotStorage(t *testing.T) {
	r := nativeSeedRetirementFixture(t)
	backup, err := nativeSeedBackup(r)
	if err != nil {
		t.Fatal(err)
	}
	if value(backup, "spec", "snapshotVolumes") != false || value(backup, "spec", "snapshotMoveData") != false ||
		value(backup, "spec", "defaultVolumesToFsBackup") != false || value(backup, "spec", "includeClusterResources") != false ||
		text(backup, "spec", "storageLocation") != "default" || text(backup, "spec", "ttl") != "168h0m0s" ||
		text(backup, "spec", "labelSelector", "matchLabels", "storage.platform.devantler.tech/benchmark-run") != r.Run {
		t.Fatal("the source backup escaped the opted-in filesystem-only disposable trial")
	}
	for _, fault := range []string{"another workload", "another namespace", "incomplete source"} {
		t.Run(fault, func(t *testing.T) {
			r := nativeSeedRetirementFixture(t)
			switch fault {
			case "another workload":
				r.Workload = benchmarkWorkload()
			case "another namespace":
				r.Namespace = "application"
			case "incomplete source":
				delete(r.Resources, "LonghornVolume")
			}
			if _, err := nativeSeedBackup(r); err == nil {
				t.Fatal("the backup admitted a foreign or incomplete source")
			}
		})
	}
}

func TestNativeSeedBackupProofMustRemainIntactBeforeSourceRetirement(t *testing.T) {
	for _, fault := range []string{"", "replacement backup", "different backup spec", "snapshot policy", "broad selector", "warnings", "changed snapshot", "replacement transfer", "replacement repository", "changed repository spec", "different pod", "missing transfer", "duplicate transfer", "duplicate repository"} {
		t.Run(fault, func(t *testing.T) {
			r, state, parent, pod, transfer, repository := retainedSeedBackupFixture(t)
			transfers, repositories := []object{transfer}, []object{repository}
			switch fault {
			case "replacement backup":
				parent["metadata"].(object)["uid"] = "replacement"
			case "different backup spec":
				parent["spec"].(object)["includedNamespaces"] = []any{"*"}
			case "snapshot policy":
				parent["spec"].(object)["resourcePolicy"] = object{"kind": "ConfigMap", "name": "velero-volume-policies"}
			case "broad selector":
				parent["spec"].(object)["orLabelSelectors"] = []any{object{}}
			case "warnings":
				parent["status"].(object)["warnings"] = float64(1)
			case "changed snapshot":
				transfer["status"].(object)["snapshotID"] = "other"
			case "replacement transfer":
				transfer["metadata"].(object)["uid"] = "replacement"
			case "replacement repository":
				repository["metadata"].(object)["uid"] = "replacement"
			case "changed repository spec":
				repository["spec"].(object)["repositoryConfig"] = object{"other": "value"}
			case "different pod":
				pod["metadata"].(object)["uid"] = "replacement"
			case "missing transfer":
				transfers = nil
			case "duplicate transfer":
				transfers = append(transfers, transfer)
			case "duplicate repository":
				repositories = append(repositories, repository)
			}
			if err := verifyRetainedSeedBackup(r, state, parent, pod, transfers, repositories); (err == nil) != (fault == "") {
				t.Fatal("source retirement accepted missing or replaced remote backup evidence", err)
			}
		})
	}
}

func TestNativeSeedBackupReadbackRejectsChangedDataSelection(t *testing.T) {
	r, state, backup, _, _, _ := retainedSeedBackupFixture(t)
	desired, err := nativeSeedBackup(r)
	if err != nil || verifySeedBackupObject(backup, desired, state.Backup.UID) != nil {
		t.Fatal("the original backup profile was refused", err)
	}
	for key, replacement := range map[string]any{"snapshotVolumes": true, "snapshotMoveData": true, "defaultVolumesToFsBackup": true, "includeClusterResources": true,
		"storageLocation": "other", "includedResources": []any{"*"}, "labelSelector": object{}} {
		t.Run(key, func(t *testing.T) {
			body, err := json.Marshal(backup)
			if err != nil {
				t.Fatal(err)
			}
			var changed object
			if json.Unmarshal(body, &changed) != nil {
				t.Fatal("invalid fixture")
			}
			changed["spec"].(object)[key] = replacement
			if verifySeedBackupObject(changed, desired, state.Backup.UID) == nil {
				t.Fatal("the native backup widened its reviewed data path")
			}
		})
	}
}
