package main

import "testing"

func nativeRestoreStorageFixture(t *testing.T) (benchmarkRecord, canarySeedState, nativeRestoreState, object, object, object, object) {
	t.Helper()
	r, seed, state := nativeRestoreStateFixture(t, "storage-bound")
	assets, err := nativeRestoreAssets(r, seed, state.Run, nativeReattachFixture().Targets)
	if err != nil {
		t.Fatal(err)
	}
	var claim, class object
	for _, asset := range assets {
		kind := text(asset, "kind")
		if kind == "StorageClass" || kind == "PersistentVolumeClaim" {
			asset["metadata"].(object)["uid"] = state.Resources[kind].UID
			asset["metadata"].(object)["resourceVersion"] = "9"
		}
		switch kind {
		case "StorageClass":
			class = asset
		case "PersistentVolumeClaim":
			claim = asset
		}
	}
	claim["spec"].(object)["volumeName"] = state.Resources["PersistentVolume"].Name
	claim["spec"].(object)["volumeMode"] = "Filesystem"
	claim["status"] = object{"phase": "Bound"}
	meta := func(kind string) object {
		id := state.Resources[kind]
		return object{"name": id.Name, "namespace": id.Namespace, "uid": id.UID, "resourceVersion": "9"}
	}
	pv := object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": meta("PersistentVolume"),
		"spec": object{"persistentVolumeReclaimPolicy": "Retain", "storageClassName": text(class, "metadata", "name"), "volumeMode": "Filesystem",
			"accessModes": []any{"ReadWriteOnce"}, "capacity": object{"storage": "4Gi"},
			"claimRef": object{"uid": text(claim, "metadata", "uid"), "namespace": benchmarkNamespace, "name": "fio-data"},
			"csi":      object{"driver": "driver.longhorn.io", "volumeHandle": state.Resources["LonghornVolume"].Name, "fsType": "ext4"}}}
	volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": meta("LonghornVolume"),
		"spec": object{"dataEngine": "v2", "size": "4294967296", "encrypted": false, "accessMode": "rwo", "numberOfReplicas": float64(3),
			"nodeSelector": []any{text(class, "metadata", "name")}, "fromBackup": "", "dataSource": "", "backingImage": ""},
		"status": object{"restoreRequired": false, "kubernetesStatus": object{"pvName": text(pv, "metadata", "name"), "namespace": benchmarkNamespace, "pvcName": "fio-data"}}}
	return r, seed, state, claim, class, pv, volume
}

func TestNativeRestoreRequiresFreshOwnedV2Storage(t *testing.T) {
	cases := map[string]func(benchmarkRecord, *nativeRestoreState, object, object, object, object){
		"original backend name": func(r benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["metadata"].(object)["name"] = r.Resources["LonghornVolume"].Name
		},
		"borrowed source identity": func(r benchmarkRecord, s *nativeRestoreState, c, _, _, _ object) {
			id := s.Resources["PersistentVolumeClaim"]
			id.UID = r.Resources["PersistentVolumeClaim"].UID
			s.Resources["PersistentVolumeClaim"] = id
			c["metadata"].(object)["uid"] = id.UID
		},
		"unrecorded binding": func(_ benchmarkRecord, s *nativeRestoreState, _, _, _, _ object) {
			s.Phase = "claim-owned"
			delete(s.Resources, "PersistentVolume")
			delete(s.Resources, "LonghornVolume")
		},
		"V1 backend": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["dataEngine"] = "v1"
		},
		"V1 class": func(_ benchmarkRecord, _ *nativeRestoreState, _, s, _, _ object) {
			s["parameters"].(object)["dataEngine"] = "v1"
		},
		"other replica targets": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["nodeSelector"] = []any{"other-workers"}
		},
		"two copies": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["numberOfReplicas"] = float64(2)
		},
		"snapshot source": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["spec"].(object)["dataSource"] = object{"kind": "VolumeSnapshot", "name": "old-source"}
		},
		"cross namespace source": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["spec"].(object)["dataSourceRef"] = object{"kind": "PersistentVolumeClaim", "name": "application", "namespace": "other"}
		},
		"Longhorn backup source": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["fromBackup"] = "backup://old-source"
		},
		"Longhorn clone": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["dataSource"] = "volume://old-source"
		},
		"linked clone": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["cloneMode"] = "linked-clone"
		},
		"backing image": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["backingImage"] = "application-image"
		},
		"native restore in progress": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["status"].(object)["restoreRequired"] = true
		},
		"deleted retained PV": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["metadata"].(object)["deletionTimestamp"] = "2026-11-15T11:00:00Z"
		},
		"delete reclaim": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["persistentVolumeReclaimPolicy"] = "Delete"
		},
		"foreign claim reference": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["claimRef"].(object)["uid"] = "application-claim"
		},
		"old backend handle": func(r benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["csi"].(object)["volumeHandle"] = r.Resources["LonghornVolume"].Name
		},
		"other CSI driver": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["csi"].(object)["driver"] = "csi.hetzner.cloud"
		},
		"unbound claim": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["status"].(object)["phase"] = "Pending"
		},
		"block claim": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["spec"].(object)["volumeMode"] = "Block"
		},
		"block PV": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["volumeMode"] = "Block"
		},
		"other filesystem": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, p, _ object) {
			p["spec"].(object)["csi"].(object)["fsType"] = "xfs"
		},
		"shared claim": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["spec"].(object)["accessModes"] = []any{"ReadWriteMany"}
		},
		"encrypted backend": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["encrypted"] = true
		},
		"wrong backend capacity": func(_ benchmarkRecord, _ *nativeRestoreState, _, _, _, v object) {
			v["spec"].(object)["size"] = "8589934592"
		},
		"wrong claim capacity": func(_ benchmarkRecord, _ *nativeRestoreState, c, _, _, _ object) {
			c["spec"].(object)["resources"].(object)["requests"].(object)["storage"] = "8Gi"
		},
		"delayed binding": func(_ benchmarkRecord, _ *nativeRestoreState, _, s, _, _ object) {
			s["volumeBindingMode"] = "WaitForFirstConsumer"
		},
	}
	r, seed, state, claim, class, pv, volume := nativeRestoreStorageFixture(t)
	if err := verifyNativeRestoreStorage(r, seed, state, claim, class, pv, volume); err != nil {
		t.Fatal("the fresh V2 binding was refused", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, seed, state, claim, class, pv, volume := nativeRestoreStorageFixture(t)
			mutate(r, &state, claim, class, pv, volume)
			if verifyNativeRestoreStorage(r, seed, state, claim, class, pv, volume) == nil {
				t.Fatal("restore accepted a non-fresh or unowned V2 binding")
			}
		})
	}
	for _, kind := range []string{"PersistentVolumeClaim", "StorageClass", "PersistentVolume", "LonghornVolume"} {
		t.Run("replacement "+kind, func(t *testing.T) {
			r, seed, state, claim, class, pv, volume := nativeRestoreStorageFixture(t)
			actual := map[string]object{"PersistentVolumeClaim": claim, "StorageClass": class, "PersistentVolume": pv, "LonghornVolume": volume}[kind]
			actual["metadata"].(object)["uid"] = "replacement"
			if verifyNativeRestoreStorage(r, seed, state, claim, class, pv, volume) == nil {
				t.Fatal("restore adopted a replacement " + kind)
			}
		})
	}
}
