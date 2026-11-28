package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

func TestOpenBaoRebindUsesTheReleasedDestinationAndKeepsTheProviderSource(t *testing.T) {
	for _, fault := range []string{
		"", "source still bound", "source reclaim changed", "source claim changed", "wrong destination volume", "staging still bound",
		"replacement claim changed", "replacement has a different owner", "replacement already bound",
	} {
		t.Run(fault, func(t *testing.T) {
			stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
			pair, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
			if err != nil {
				t.Fatal(err)
			}
			oldPV := pvs[0]
			valueObject(oldPV, "status")["phase"] = "Released"
			raw, err := json.Marshal(oldPV)
			if err != nil {
				t.Fatal(err)
			}
			var stagedPV object
			if err = json.Unmarshal(raw, &stagedPV); err != nil {
				t.Fatal(err)
			}
			const stagedUID = "70eaa3f7-5657-40d8-9e80-06f41925c4aa"
			const pvUID = "4e7865d4-0e47-472d-a23f-987a5f0474b7"
			const dataUID = "42cef7ec-2e49-4ee8-bd01-c32d79941cb0"
			const replacementUID = "037aebda-5acc-4271-8062-98c01318feab"
			stagedName := "pvc-" + stagedUID
			valueObject(stagedPV, "metadata")["name"] = stagedName
			valueObject(stagedPV, "metadata")["uid"] = pvUID
			valueObject(stagedPV, "spec")["storageClassName"] = "longhorn-v2"
			valueObject(stagedPV, "spec")["csi"] = object{"driver": "driver.longhorn.io", "volumeHandle": stagedName}
			valueObject(stagedPV, "spec")["claimRef"] = object{"name": "lh-openbao-staging-data-123", "namespace": openBaoNamespace, "uid": stagedUID}
			volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
				"metadata": object{"name": stagedName, "namespace": "longhorn-system", "uid": dataUID, "resourceVersion": "12"},
				"spec":     object{"dataEngine": "v2", "numberOfReplicas": float64(3), "size": "10737418240"},
				"status":   object{"state": "detached"},
			}
			intent := openBaoStorageRebind{
				Source: pair.Disks[0], StagingClaim: benchmarkResource{"lh-openbao-staging-data-123", openBaoNamespace, stagedUID},
				DestinationPV: benchmarkIdentity(stagedPV), DestinationVolume: benchmarkIdentity(volume),
				Run: "123", SourceSHA: "acef1536f21e1b8cf32d62c6045fc989fa8e55d6",
			}
			replacement := intent.replacementClaim()
			valueObject(replacement, "metadata")["uid"] = replacementUID
			valueObject(replacement, "metadata")["resourceVersion"] = "11"
			replacement["status"] = object{"phase": "Pending"}
			switch fault {
			case "source still bound":
				valueObject(oldPV, "status")["phase"] = "Bound"
			case "source reclaim changed":
				valueObject(oldPV, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			case "source claim changed":
				valueObject(oldPV, "spec", "claimRef")["uid"] = stagedUID
			case "wrong destination volume":
				valueObject(volume, "metadata")["uid"] = replacementUID
			case "staging still bound":
				valueObject(stagedPV, "status")["phase"] = "Bound"
			case "replacement claim changed":
				valueObject(replacement, "spec")["volumeName"] = intent.Source.PV.Name
			case "replacement has a different owner":
				valueObject(replacement, "metadata", "annotations")[openBaoVolumeRebindOwner] = "other"
			case "replacement already bound":
				valueObject(replacement, "status")["phase"] = "Bound"
			}
			patch, err := intent.patch(oldPV, stagedPV, volume, replacement)
			if fault != "" {
				if err == nil {
					t.Fatal("the rebind accepted an ambiguous or changed storage identity")
				}
				return
			}
			expected := []object{
				{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
				{"op": "test", "path": "/spec/claimRef", "value": value(stagedPV, "spec", "claimRef")},
				{"op": "replace", "path": "/spec/claimRef", "value": object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
					"namespace": openBaoNamespace, "name": "data-openbao-1", "uid": replacementUID}},
			}
			if err != nil || !reflect.DeepEqual(patch, expected) {
				t.Fatalf("the guarded rebind delta was not proven: %+v, %v", patch, err)
			}
		})
	}
}

func openBaoReboundPairFixture(t *testing.T) [2]openBaoReboundStorage {
	t.Helper()
	stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
	source, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
	if err != nil {
		t.Fatal(err)
	}
	var pair [2]openBaoReboundStorage
	for index, kind := range [2]string{"data", "audit"} {
		before := source.Disks[index]
		original := pvs[index]
		valueObject(original, "status")["phase"] = "Released"
		uid := func(offset int) string { return fmt.Sprintf("b286e4f7-51db-4bc1-aa4d-%012d", offset+index) }
		name := "pvc-" + uid(40)
		intent := openBaoStorageRebind{
			Source: before, StagingClaim: benchmarkResource{Name: "lh-openbao-staging-" + kind + "-123", Namespace: openBaoNamespace, UID: uid(40)},
			DestinationPV:     benchmarkResource{Name: name, UID: uid(50)},
			DestinationVolume: benchmarkResource{Name: name, Namespace: "longhorn-system", UID: uid(60)},
			Run:               "123", SourceSHA: "acef1536f21e1b8cf32d62c6045fc989fa8e55d6",
		}
		claim := intent.replacementClaim()
		valueObject(claim, "metadata")["uid"] = uid(70)
		valueObject(claim, "metadata")["resourceVersion"] = "12"
		size := strconv.FormatUint(before.Bytes>>30, 10) + "Gi"
		claim["status"] = object{"phase": "Bound", "capacity": object{"storage": size}}
		destination := object{
			"apiVersion": "v1", "kind": "PersistentVolume", "metadata": object{"name": name, "uid": uid(50), "resourceVersion": "18"},
			"status": object{"phase": "Bound"},
			"spec": object{"storageClassName": "longhorn-v2", "persistentVolumeReclaimPolicy": "Retain",
				"claimRef":   object{"namespace": openBaoNamespace, "name": before.PVC.Name, "uid": uid(70)},
				"volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"},
				"capacity": object{"storage": size}, "csi": object{"driver": "driver.longhorn.io", "volumeHandle": name}},
		}
		backend := object{
			"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
			"metadata": object{"name": name, "namespace": "longhorn-system", "uid": uid(60), "resourceVersion": "19"},
			"status":   object{"state": "detached"},
			"spec":     object{"dataEngine": "v2", "numberOfReplicas": float64(3), "size": strconv.FormatUint(before.Bytes, 10)},
		}
		pair[index] = openBaoReboundStorage{Intent: intent, Original: original, Dest: destination, Backend: backend, NewClaim: claim}
	}
	return pair
}

func TestOpenBaoMemberRemainsFencedUntilBothDestinationsBind(t *testing.T) {
	pair := openBaoReboundPairFixture(t)
	successors := [2]benchmarkResource{benchmarkIdentity(pair[0].NewClaim), benchmarkIdentity(pair[1].NewClaim)}
	if err := verifyOpenBaoReboundPair("openbao-1", pair, successors); err != nil {
		t.Fatalf("the exact data and audit destinations were refused: %v", err)
	}
	valueObject(pair[1].NewClaim, "status")["phase"] = "Pending"
	if err := verifyOpenBaoReboundPair("openbao-1", pair, successors); err == nil {
		t.Fatal("a bound data PVC opened the fence before the audit PVC bound")
	}
	valueObject(pair[1].NewClaim, "status")["phase"] = "Bound"
	changed := "b286e4f7-51db-4bc1-aa4d-000000000999"
	valueObject(pair[1].NewClaim, "metadata")["uid"] = changed
	valueObject(pair[1].Dest, "spec", "claimRef")["uid"] = changed
	if err := verifyOpenBaoReboundPair("openbao-1", pair, successors); err == nil {
		t.Fatal("a replacement claim outside the committed pair opened the fence")
	}
	valueObject(pair[1].NewClaim, "metadata")["uid"] = successors[1].UID
	valueObject(pair[1].Dest, "spec", "claimRef")["uid"] = pair[1].Intent.StagingClaim.UID
	if err := verifyOpenBaoReboundPair("openbao-1", pair, successors); err == nil {
		t.Fatal("the claimed audit volume still belonged to the old staging claim")
	}
}

func TestOpenBaoDoesNotReplayAnAlreadyCommittedClaimRebind(t *testing.T) {
	pair := openBaoReboundPairFixture(t)
	receipt := benchmarkIdentity(pair[1].NewClaim)
	ready, patch, err := pair[1].rebindStep(receipt)
	if err != nil || !ready || patch != nil {
		t.Fatalf("the already bound destination was not recognized: ready=%t patch=%v err=%v", ready, patch, err)
	}
	valueObject(pair[1].NewClaim, "status")["phase"] = "Pending"
	valueObject(pair[1].Dest, "status")["phase"] = "Released"
	ready, patch, err = pair[1].rebindStep(receipt)
	if err != nil || ready || patch != nil {
		t.Fatalf("the committed rebind should wait for Bound: ready=%t patch=%v err=%v", ready, patch, err)
	}
	other := receipt
	other.UID = "b286e4f7-51db-4bc1-aa4d-000000000999"
	if _, _, err := pair[1].rebindStep(other); err == nil {
		t.Fatal("an unrecorded replacement claim was accepted")
	}
	valueObject(pair[1].Dest, "spec", "claimRef")["uid"] = other.UID
	if _, _, err := pair[1].rebindStep(receipt); err == nil {
		t.Fatal("an already claimed foreign destination was accepted")
	}
}
