package main

import (
	"reflect"
	"testing"
)

func TestKeeperRebindOnlyChangesThePreparedPV(t *testing.T) {
	originalClaim := benchmarkResource{Name: "data-coroot-clickhouse-keeper-1", Namespace: corootKeeperNamespace, UID: "d827f800-8471-40c6-815a-7e59617ee620"}
	stagedClaim := benchmarkResource{Name: corootKeeperStagingPrefix + "991", Namespace: corootKeeperNamespace, UID: "c50ba448-28ed-458c-8dd7-78355a07a616"}
	originalPV := benchmarkResource{Name: "pvc-" + originalClaim.UID, UID: "2c27e0ad-f1af-4909-b100-391c978e02b4"}
	stagedPV := benchmarkResource{Name: "pvc-" + stagedClaim.UID, UID: "7860ea13-bc1d-4e4c-9a12-ed34c67dbbed"}
	backend := benchmarkResource{Name: stagedPV.Name, Namespace: "longhorn-system", UID: "bbbbfbc3-62b5-4a60-a982-55f6c7632464"}
	coroot := benchmarkResource{Name: "coroot", Namespace: corootKeeperNamespace, UID: "a4f66240-e801-4624-a98a-af06d7fbfb2f"}
	plan := corootKeeperRebind{
		Coroot:      coroot,
		Original:    corootKeeperProviderDisk{Claim: originalClaim, PV: originalPV, Handle: "291118"},
		StagedClaim: stagedClaim, StagedPV: stagedPV, StagedVolume: backend,
	}
	native := func(apiVersion, kind string, resource benchmarkResource) object {
		return object{"apiVersion": apiVersion, "kind": kind, "metadata": object{
			"name": resource.Name, "namespace": resource.Namespace, "uid": resource.UID, "resourceVersion": "54",
		}}
	}
	volume := func(pv, claim benchmarkResource, class, driver, handle, size string) object {
		obj := native("v1", "PersistentVolume", pv)
		obj["status"] = object{"phase": "Released"}
		obj["spec"] = object{
			"storageClassName": class, "persistentVolumeReclaimPolicy": "Retain",
			"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem", "capacity": object{"storage": size},
			"claimRef": object{"namespace": claim.Namespace, "name": claim.Name, "uid": claim.UID},
			"csi":      object{"driver": driver, "volumeHandle": handle},
		}
		return obj
	}
	owner := native("coroot.com/v1", "Coroot", coroot)
	original := volume(originalPV, originalClaim, "hcloud", hetznerCSIDriver, "291118", "10Gi")
	staged := volume(stagedPV, stagedClaim, "longhorn-v2", "driver.longhorn.io", backend.Name, "2Gi")
	disk := native("longhorn.io/v1beta2", "Volume", backend)
	disk["spec"] = object{"dataEngine": "v2", "size": corootKeeperDataBytes, "numberOfReplicas": float64(3)}
	disk["status"] = object{"state": "detached"}
	replacement, err := plan.replacementClaim(owner, original, staged, disk)
	if err != nil || text(replacement, "spec", "volumeName") != stagedPV.Name || text(replacement, "spec", "storageClassName") != "longhorn-v2" {
		t.Fatalf("invalid prebound claim: %v", err)
	}
	newUID := "888eecda-7bd8-4b90-89f1-49a21bab5654"
	metadata := valueObject(replacement, "metadata")
	metadata["uid"], metadata["resourceVersion"] = newUID, "81"
	replacement["status"] = object{"phase": "Pending"}
	patch, err := plan.rebindChanges(owner, original, staged, disk, replacement)
	if err != nil || len(patch) != 3 || text(patch[0], "value") != stagedClaim.UID ||
		!reflect.DeepEqual(patch[2], object{"op": "replace", "path": "/spec/claimRef", "value": object{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": corootKeeperNamespace,
			"name": originalClaim.Name, "uid": newUID,
		}}) {
		t.Fatalf("unexpected prepared-PV rebind patch: %v", err)
	}
	for _, tt := range []struct {
		name   string
		target object
		key    string
		bad    any
	}{
		{name: "original Retain policy lost", target: valueObject(original, "spec"), key: "persistentVolumeReclaimPolicy", bad: "Delete"},
		{name: "original binding moved", target: valueObject(original, "spec", "claimRef"), key: "uid", bad: newUID},
		{name: "destination has a writer", target: valueObject(disk, "status"), key: "state", bad: "attached"},
		{name: "old claim was replayed", target: metadata, key: "uid", bad: originalClaim.UID},
		{name: "new claim targets an original", target: valueObject(replacement, "spec"), key: "volumeName", bad: originalPV.Name},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.target[tt.key]
			tt.target[tt.key] = tt.bad
			defer func() { tt.target[tt.key] = before }()
			if _, err := plan.rebindChanges(owner, original, staged, disk, replacement); err == nil {
				t.Fatal("rebind accepted a changed storage identity")
			}
		})
	}
}
