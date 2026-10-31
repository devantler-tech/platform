package main

import "errors"

func backupManifest(i invocation) object {
	o := recoveryManifest(i, "Backup", i.namespace+"-backup", "velero", "velero.io/v1")
	o["spec"] = object{
		"includedNamespaces": []any{"private-mail"}, "includedResources": []any{"pods"}, "includeClusterResources": false,
		"labelSelector":   object{"matchLabels": object{"statefulset.kubernetes.io/pod-name": "private-mail-0"}},
		"snapshotVolumes": false, "defaultVolumesToFsBackup": true, "storageLocation": "default", "ttl": "24h0m0s",
	}
	return o
}
func downloadManifest(i invocation, snapshot string) object {
	o := recoveryManifest(i, "DataDownload", i.namespace+"-download", "velero", "velero.io/v2alpha1")
	o["spec"] = object{"targetVolume": object{"pvc": "data", "namespace": i.namespace, "pv": ""}, "backupStorageLocation": "default", "datamover": "velero", "snapshotID": snapshot, "sourceNamespace": "private-mail", "operationTimeout": "10m0s", "nodeOS": "linux"}
	return o
}
func proveRestoredStorage(pvc, pv, volume object, i invocation, claimUID, sourceVolume string) error {
	ps, vs := child(pvc, "spec"), child(pv, "spec")
	pm, vm := child(pv, "metadata"), child(volume, "metadata")
	name := text(pm, "name")
	ref, csi := child(vs, "claimRef"), child(vs, "csi")
	if claimUID == "" || sourceVolume == "" || name == "" || name == sourceVolume || text(pm, "uid") == "" || text(child(pvc, "metadata"), "uid") != claimUID || text(child(pvc, "status"), "phase") != "Bound" || text(ps, "storageClassName") != i.namespace || text(ps, "volumeName") != name || text(ref, "uid") != claimUID || text(ref, "name") != "data" || text(ref, "namespace") != i.namespace || text(vs, "persistentVolumeReclaimPolicy") != "Delete" || text(csi, "driver") != "driver.longhorn.io" || text(csi, "volumeHandle") != name || text(vm, "name") != name || text(vm, "uid") == "" {
		return errors.New("restored-storage-identity")
	}
	spec := child(volume, "spec")
	if spec["encrypted"] != true || spec["numberOfReplicas"] != float64(3) || text(spec, "dataEngine") != "v1" {
		return errors.New("restored-storage-encryption")
	}
	return nil
}
