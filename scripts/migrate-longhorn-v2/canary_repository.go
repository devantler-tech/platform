package main

import (
	"errors"
	"reflect"
)

// Velero v1.18.4 selects Kopia repositories by this label tuple. Its Restic
// identifier is empty for Kopia. Bind the supported key, ready native object
// and spec, retaining that same repository across backup and restore.
func (p *canaryRun) verifyFilesystemTransfer(repositories, transfers []object, parent, pod object, expectedSnapshot, sourceNS string) (string, error) {
	if sourceNS == "" || text(parent, "kind") == "Backup" && text(pod, "metadata", "namespace") != sourceNS || text(parent, "kind") == "Restore" && p.kopiaRepository == nil {
		return "", errors.New("canary Kopia source repository is unbound")
	}
	var selected object
	for _, repository := range repositories {
		if text(repository, "spec", "volumeNamespace") != sourceNS && text(repository, "metadata", "labels", "velero.io/volume-namespace") != sourceNS {
			continue
		}
		if selected != nil || text(repository, "apiVersion") != "velero.io/v1" || text(repository, "kind") != "BackupRepository" ||
			text(repository, "metadata", "namespace") != "velero" || text(repository, "metadata", "name") == "" || text(repository, "metadata", "uid") == "" || text(repository, "metadata", "resourceVersion") == "" || value(repository, "metadata", "deletionTimestamp") != nil ||
			text(repository, "metadata", "labels", "velero.io/volume-namespace") != sourceNS || text(repository, "metadata", "labels", "velero.io/storage-location") != "default" || text(repository, "metadata", "labels", "velero.io/repository-type") != "kopia" ||
			text(repository, "spec", "volumeNamespace") != sourceNS || text(repository, "spec", "backupStorageLocation") != "default" || text(repository, "spec", "repositoryType") != "kopia" || text(repository, "spec", "resticIdentifier") != "" || text(repository, "status", "phase") != "Ready" {
			return "", errors.New("canary Kopia repository key, identity or readiness is unproven")
		}
		selected = repository
	}
	if selected == nil || p.kopiaRepository != nil && (benchmarkIdentity(selected) != benchmarkIdentity(p.kopiaRepository) || !reflect.DeepEqual(value(selected, "spec"), value(p.kopiaRepository, "spec"))) {
		return "", errors.New("canary Kopia repository is missing or changed")
	}
	snapshot, err := verifyCanaryTransfer(transfers, parent, pod, expectedSnapshot, sourceNS)
	if err != nil {
		return "", err
	}
	if text(parent, "kind") == "Backup" {
		p.kopiaRepository = selected
	}
	return snapshot, nil
}
