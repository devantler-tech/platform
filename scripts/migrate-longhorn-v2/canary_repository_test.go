package main

import "testing"

func canaryRepositoryFixture() object {
	repository := resource("native-kopia-repository")
	repository["apiVersion"], repository["kind"] = "velero.io/v1", "BackupRepository"
	repository["metadata"].(object)["namespace"] = "velero"
	repository["metadata"].(object)["labels"] = object{"velero.io/volume-namespace": "longhorn-canary-v1-1234", "velero.io/storage-location": "default", "velero.io/repository-type": "kopia"}
	repository["spec"] = object{"volumeNamespace": "longhorn-canary-v1-1234", "backupStorageLocation": "default", "repositoryType": "kopia"}
	repository["status"] = object{"phase": "Ready"}
	return repository
}

func TestFilesystemRecoveryUsesTheSameReadyKopiaRepository(t *testing.T) {
	var p canaryRun
	repository := canaryRepositoryFixture()
	parent, pod, transfer := transferFixture("Backup")
	if _, err := p.verifyFilesystemTransfer([]object{repository}, []object{transfer}, parent, pod, "", "longhorn-canary-v1-1234"); err != nil {
		t.Fatal("native Kopia backup without a Restic identifier was refused:", err)
	}
	parent, pod, transfer = transferFixture("Restore")
	pod["metadata"].(object)["namespace"] = "longhorn-canary-v2-1234"
	transfer["spec"].(object)["pod"].(object)["namespace"] = "longhorn-canary-v2-1234"
	current := canaryRepositoryFixture()
	current["metadata"].(object)["resourceVersion"] = "2"
	current["status"].(object)["lastMaintenanceTime"] = "2026-11-03T00:00:00Z"
	if _, err := p.verifyFilesystemTransfer([]object{current}, []object{transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err != nil {
		t.Fatal("ordinary repository status updates broke restoration:", err)
	}
	for name, mutate := range map[string]func(object){
		"replaced repository":       func(o object) { o["metadata"].(object)["uid"] = "replacement" },
		"changed repository config": func(o object) { o["spec"].(object)["repositoryConfig"] = object{"changed": "true"} },
		"wrong location":            func(o object) { o["spec"].(object)["backupStorageLocation"] = "other" },
		"wrong labels":              func(o object) { o["metadata"].(object)["labels"].(object)["velero.io/volume-namespace"] = "other" },
		"wrong source":              func(o object) { o["spec"].(object)["volumeNamespace"] = "other" },
		"wrong type":                func(o object) { o["spec"].(object)["repositoryType"] = "restic" },
		"not ready":                 func(o object) { o["status"].(object)["phase"] = "NotReady" },
		"deleting":                  func(o object) { o["metadata"].(object)["deletionTimestamp"] = "2026-11-03T00:00:00Z" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := canaryRepositoryFixture()
			mutate(candidate)
			if _, err := p.verifyFilesystemTransfer([]object{candidate}, []object{transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
				t.Fatal("repository replacement or contradictory native evidence was accepted")
			}
		})
	}
	for name, repositories := range map[string][]object{"missing": nil, "ambiguous": {repository, repository}} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.verifyFilesystemTransfer(repositories, []object{transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
				t.Fatal("incomplete repository evidence accepted")
			}
		})
	}
	var unbound canaryRun
	if _, err := unbound.verifyFilesystemTransfer([]object{repository}, []object{transfer}, parent, pod, "snapshot", "longhorn-canary-v1-1234"); err == nil {
		t.Fatal("restore accepted without the verified backup repository")
	}
}
