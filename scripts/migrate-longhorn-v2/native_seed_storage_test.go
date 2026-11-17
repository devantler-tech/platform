package main

import "testing"

func seedStorageFixture(t *testing.T) (benchmarkRecord, object, object, object, object) {
	t.Helper()
	r := nativeSeedRetirementFixture(t)
	_, claim, class, pv, volume := recoveryStorageFixture(t)
	for kind, actual := range map[string]object{"PersistentVolumeClaim": claim, "StorageClass": class, "PersistentVolume": pv, "LonghornVolume": volume} {
		id := r.Resources[kind]
		actual["metadata"].(object)["name"] = id.Name
		actual["metadata"].(object)["uid"] = id.UID
		if id.Namespace != "" {
			actual["metadata"].(object)["namespace"] = id.Namespace
		}
	}
	claim["spec"].(object)["resources"] = object{"requests": object{"storage": "4Gi"}}
	pv["spec"].(object)["capacity"] = object{"storage": "4Gi"}
	pv["spec"].(object)["claimRef"].(object)["namespace"] = benchmarkNamespace
	volume["spec"].(object)["size"] = "4294967296"
	volume["spec"].(object)["encrypted"] = false
	volume["spec"].(object)["fromBackup"] = ""
	volume["spec"].(object)["dataSource"] = ""
	volume["spec"].(object)["backingImage"] = ""
	volume["spec"].(object)["numberOfReplicas"] = float64(3)
	volume["spec"].(object)["nodeSelector"] = []any{benchmarkTag("v1", r.Run)}
	value(volume, "status", "kubernetesStatus").(object)["namespace"] = benchmarkNamespace
	value(volume, "status", "kubernetesStatus").(object)["pvcName"] = "fio-data"
	return r, claim, class, pv, volume
}

func TestNativeSeedStorageCannotAdoptApplicationBytesOrAnotherBackend(t *testing.T) {
	for _, fault := range []string{"", "changed claim", "changed class", "changed PV", "changed backend", "restore", "clone", "linked clone", "backing image", "wrong size", "encrypted", "different replica target", "different namespace", "PVC data source"} {
		t.Run(fault, func(t *testing.T) {
			r, claim, class, pv, volume := seedStorageFixture(t)
			switch fault {
			case "changed claim":
				claim["metadata"].(object)["uid"] = "replacement"
			case "changed class":
				class["metadata"].(object)["uid"] = "replacement"
			case "changed PV":
				pv["metadata"].(object)["uid"] = "replacement"
			case "changed backend":
				volume["metadata"].(object)["uid"] = "replacement"
			case "restore":
				volume["spec"].(object)["fromBackup"] = "backup://existing-data"
			case "clone":
				volume["spec"].(object)["dataSource"] = "volume://application"
			case "linked clone":
				volume["spec"].(object)["cloneMode"] = "linked-clone"
			case "backing image":
				volume["spec"].(object)["backingImage"] = "existing-image"
			case "wrong size":
				volume["spec"].(object)["size"] = "8589934592"
			case "encrypted":
				volume["spec"].(object)["encrypted"] = true
			case "different replica target":
				volume["spec"].(object)["nodeSelector"] = []any{"other"}
			case "different namespace":
				value(volume, "status", "kubernetesStatus").(object)["namespace"] = "application"
			case "PVC data source":
				claim["spec"].(object)["dataSourceRef"] = object{"kind": "VolumeSnapshot", "name": "existing-data"}
			}
			if err := verifyNativeSeedStorage(r, claim, class, pv, volume); (err == nil) != (fault == "") {
				t.Fatal("the disposable source adopted foreign bytes or a replaced binding", err)
			}
		})
	}
}
