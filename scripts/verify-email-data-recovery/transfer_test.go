package main

import "testing"

func TestBackupAndDownloadCannotRestoreBackedUpWorkloads(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	b := backupManifest(i)
	spec := child(b, "spec")
	if !i.permitsMutation("Backup", text(child(b, "metadata"), "namespace"), text(child(b, "metadata"), "name")) || spec["snapshotVolumes"] != false || spec["defaultVolumesToFsBackup"] != true {
		t.Fatal("backup left the fixed filesystem-data path")
	}
	namespaces, _ := spec["includedNamespaces"].([]any)
	resources, _ := spec["includedResources"].([]any)
	if len(namespaces) != 1 || namespaces[0] != "private-mail" || len(resources) != 1 || resources[0] != "pods" || text(child(child(spec, "labelSelector"), "matchLabels"), "statefulset.kubernetes.io/pod-name") != "private-mail-0" {
		t.Fatal("backup can include unrelated sources or restore executable resources")
	}
	transfer := downloadManifest(i, "0123456789abcdef0123456789abcdef")
	target := child(child(transfer, "spec"), "targetVolume")
	if text(target, "namespace") != i.namespace || text(target, "pvc") != "data" || target["pv"] != "" {
		t.Fatal("download can overwrite serving storage")
	}
	if text(transfer, "kind") != "DataDownload" || text(child(transfer, "spec"), "sourceNamespace") != "private-mail" {
		t.Fatal("data transfer replays resources instead of bytes")
	}
}

func TestRestoredStorageMustBeSeparateEncryptedAndBoundToCreatedClaim(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	pvc := object{"metadata": object{"uid": "target-claim-uid"}, "spec": object{"storageClassName": i.namespace, "volumeName": "pvc-target-volume"}, "status": object{"phase": "Bound"}}
	pv := object{"metadata": object{"name": "pvc-target-volume", "uid": "target-volume-uid"}, "spec": object{"claimRef": object{"namespace": i.namespace, "name": "data", "uid": "target-claim-uid"}, "persistentVolumeReclaimPolicy": "Delete", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "pvc-target-volume"}}}
	volume := object{"metadata": object{"name": "pvc-target-volume", "uid": "target-longhorn-uid"}, "spec": object{"encrypted": true, "numberOfReplicas": float64(3), "dataEngine": "v1"}}
	if err := proveRestoredStorage(pvc, pv, volume, i, "target-claim-uid", "source-volume"); err != nil {
		t.Fatal("isolated encrypted restored volume was refused")
	}
	child(volume, "spec")["encrypted"] = false
	if proveRestoredStorage(pvc, pv, volume, i, "target-claim-uid", "source-volume") == nil {
		t.Fatal("plaintext restored storage was accepted")
	}
	child(volume, "spec")["encrypted"] = true
	child(pv, "spec")["claimRef"] = object{"namespace": "private-mail", "name": "private-mail-data", "uid": "source-claim-uid"}
	if proveRestoredStorage(pvc, pv, volume, i, "target-claim-uid", "source-volume") == nil {
		t.Fatal("serving volume was accepted as isolated storage")
	}
}
