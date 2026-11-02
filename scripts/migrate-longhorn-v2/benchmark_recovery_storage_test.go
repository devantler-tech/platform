package main

import "testing"

func recoveryStorageFixture(t *testing.T) (benchmarkRecord, object, object, object, object) {
	t.Helper()
	record := recoveryFixture(t)
	assets, _ := benchmarkResources(record.Phase, record.Run, record.Namespace)
	var class, claim object
	for _, asset := range assets {
		if text(asset, "kind") == "StorageClass" {
			class = asset
		}
		if text(asset, "kind") == "PersistentVolumeClaim" {
			claim = asset
		}
	}
	class["metadata"].(object)["uid"] = "scratch-class"
	claim["metadata"].(object)["uid"] = "scratch-claim"
	claim["spec"].(object)["volumeName"] = "scratch-pv"
	pv := resource("scratch-pv")
	pv["spec"] = object{"persistentVolumeReclaimPolicy": "Retain", "claimRef": object{"uid": "scratch-claim", "name": "fio-data", "namespace": record.Namespace}, "storageClassName": text(class, "metadata", "name"), "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "scratch-backend"}}
	volume := resource("scratch-backend")
	volume["metadata"].(object)["namespace"] = "longhorn-system"
	volume["spec"] = object{"dataEngine": "v1"}
	volume["status"] = object{"kubernetesStatus": object{"pvName": "scratch-pv"}}
	record.Resources = map[string]benchmarkResource{"StorageClass": benchmarkIdentity(class), "PersistentVolumeClaim": benchmarkIdentity(claim), "PersistentVolume": benchmarkIdentity(pv), "LonghornVolume": benchmarkIdentity(volume)}
	return record, claim, class, pv, volume
}

func TestRecoveredStorageMustRetainItsExactOriginalUIDGraph(t *testing.T) {
	record, claim, class, pv, volume := recoveryStorageFixture(t)
	if err := verifyRecoveredBenchmarkStorage(record, claim, class, pv, volume); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"claim replaced", "class replaced", "PV replaced", "backend replaced", "other claim", "delete reclaim", "other engine", "other class parameters", "missing original backend UID", "unresolved create"} {
		t.Run(change, func(t *testing.T) {
			r, c, s, p, v := recoveryStorageFixture(t)
			switch change {
			case "claim replaced":
				c["metadata"].(object)["uid"] = "customer-claim"
			case "class replaced":
				s["metadata"].(object)["uid"] = "customer-class"
			case "PV replaced":
				p["metadata"].(object)["uid"] = "customer-pv"
			case "backend replaced":
				v["metadata"].(object)["uid"] = "customer-backend"
			case "other claim":
				p["spec"].(object)["claimRef"].(object)["uid"] = "customer-claim"
			case "delete reclaim":
				p["spec"].(object)["persistentVolumeReclaimPolicy"] = "Delete"
			case "other engine":
				v["spec"].(object)["dataEngine"] = "v2"
			case "other class parameters":
				s["parameters"].(object)["nodeSelector"] = "customer-nodes"
			case "missing original backend UID":
				delete(r.Resources, "LonghornVolume")
			case "unresolved create":
				r.PendingCreate = "Pod"
			}
			if verifyRecoveredBenchmarkStorage(r, c, s, p, v) == nil {
				t.Fatal("foreign or unproven storage graph accepted")
			}
		})
	}
}
