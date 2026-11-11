package main

import (
	"encoding/json"
	"testing"
)

func retainedVolumeFixture(t *testing.T) (object, object, object, sourceIdentity, retainedVolumeWitness) {
	t.Helper()
	graph, readerUID, claimUID := readerInputFixture(t)
	reader, claim := graph[7], graph[6]
	name := "pvc-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	child(claim, "spec")["volumeName"] = name
	witness := retainedVolumeWitness{readerUID, claimUID, "99999999-1111-2222-3333-444444444444"}
	volume := object{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": object{"name": name, "uid": witness.PVUID, "resourceVersion": "500"},
		"spec": object{
			"claimRef":         object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "data", "namespace": retainedRecovery.namespace, "uid": claimUID},
			"storageClassName": retainedRecovery.namespace, "persistentVolumeReclaimPolicy": "Delete",
			"csi": object{"driver": "driver.longhorn.io", "volumeHandle": name, "volumeAttributes": object{"encrypted": "true"}, "nodeStageSecretRef": object{"name": "storage-key", "namespace": retainedRecovery.namespace}},
		},
		"status": object{"phase": "Bound"},
	}
	source := sourceIdentity{podUID: "serving-pod", claimUID: "ffffffff-1111-2222-3333-444444444444", pv: "pvc-serving-volume", pvUID: "serving-pv"}
	return reader, claim, volume, source, witness
}

func TestRetainedVolumeQualificationAcceptsTheRestoredBindingWithoutChangingIt(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			reader, claim, volume, source, witness := retainedVolumeFixture(t)
			child(volume, "spec")["persistentVolumeReclaimPolicy"] = policy
			before, _ := json.Marshal([]object{reader, claim, volume})
			if err := qualifyRetainedVolume(reader, claim, volume, source, fixtureImage, witness); err != nil {
				t.Fatalf("the independently bound restored PV was refused: %v", err)
			}
			after, _ := json.Marshal([]object{reader, claim, volume})
			if string(before) != string(after) || text(child(volume, "metadata"), "name") == "pvc-"+witness.ClaimUID {
				t.Fatal("qualification modified inputs or relied on the original-provisioning UID formula")
			}
		})
	}
}

func TestRetainedVolumeQualificationRejectsChangedOrForeignBindings(t *testing.T) {
	for _, change := range []string{"reader-witness", "claim-witness", "pv-witness", "same-witness", "reader-uid", "claim-uid", "pv-uid", "reader-version", "pv-version", "pv-terminating", "claim-terminating", "reader-terminating", "reader-command", "reader-not-ready", "reader-owner", "source-claim", "source-volume", "source-pv-uid", "missing-source", "claim-namespace", "claim-unbound", "pv-name", "pv-kind", "pv-unbound", "claim-reference", "foreign-reference", "driver", "handle", "unencrypted", "key-reference", "foreign-key-namespace", "unknown-policy", "storage-class"} {
		t.Run(change, func(t *testing.T) {
			reader, claim, volume, source, witness := retainedVolumeFixture(t)
			spec := child(volume, "spec")
			switch change {
			case "reader-witness":
				witness.ReaderUID = ""
			case "claim-witness":
				witness.ClaimUID = "foreign"
			case "pv-witness":
				witness.PVUID = ""
			case "same-witness":
				witness.PVUID = witness.ClaimUID
			case "reader-uid":
				child(reader, "metadata")["uid"] = "replacement"
			case "claim-uid":
				child(claim, "metadata")["uid"] = "replacement"
			case "pv-uid":
				child(volume, "metadata")["uid"] = "replacement"
			case "reader-version":
				delete(child(reader, "metadata"), "resourceVersion")
			case "pv-version":
				child(volume, "metadata")["resourceVersion"] = "invalid"
			case "pv-terminating":
				child(volume, "metadata")["deletionTimestamp"] = "2026-11-11T00:00:00Z"
			case "claim-terminating":
				child(claim, "metadata")["deletionTimestamp"] = "2026-11-11T00:00:00Z"
			case "reader-terminating":
				child(reader, "metadata")["deletionTimestamp"] = "2026-11-11T00:00:00Z"
			case "reader-command":
				items(child(reader, "spec"), "containers")[0]["command"] = []any{"foreign"}
			case "reader-not-ready":
				reader["status"] = object{"phase": "Pending"}
			case "reader-owner":
				child(reader, "metadata")["ownerReferences"] = []any{object{"uid": "foreign"}}
			case "source-claim":
				source.claimUID = witness.ClaimUID
			case "source-volume":
				source.pv = text(child(volume, "metadata"), "name")
			case "source-pv-uid":
				source.pvUID = witness.PVUID
			case "missing-source":
				source = sourceIdentity{}
			case "claim-namespace":
				child(claim, "metadata")["namespace"] = "private-mail"
			case "claim-unbound":
				child(claim, "status")["phase"] = "Pending"
			case "pv-name":
				child(volume, "metadata")["name"] = "pvc-foreign"
			case "pv-kind":
				volume["kind"] = "Secret"
			case "pv-unbound":
				child(volume, "status")["phase"] = "Released"
			case "claim-reference":
				child(spec, "claimRef")["uid"] = source.claimUID
			case "foreign-reference":
				child(spec, "claimRef")["namespace"] = "private-mail"
			case "driver":
				child(spec, "csi")["driver"] = "foreign.invalid"
			case "handle":
				child(spec, "csi")["volumeHandle"] = source.pv
			case "unencrypted":
				child(child(spec, "csi"), "volumeAttributes")["encrypted"] = "false"
			case "key-reference":
				child(child(spec, "csi"), "nodeStageSecretRef")["name"] = "foreign"
			case "foreign-key-namespace":
				child(child(spec, "csi"), "nodeStageSecretRef")["namespace"] = "private-mail"
			case "unknown-policy":
				spec["persistentVolumeReclaimPolicy"] = "Recycle"
			case "storage-class":
				spec["storageClassName"] = "foreign"
			}
			if qualifyRetainedVolume(reader, claim, volume, source, fixtureImage, witness) == nil {
				t.Fatal("changed or foreign restored binding was accepted")
			}
		})
	}
}
