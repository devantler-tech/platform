package main

import (
	"encoding/json"
	"reflect"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

func TestUmamiStandbyRetentionChangesOnlyControllerOwnership(t *testing.T) {
	for _, fault := range []string{"", "replacement UID", "became primary", "extra owner", "wrong instance label", "unready claim", "lost Retain"} {
		t.Run(fault, func(t *testing.T) {
			observation, pod, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
			cluster := observation.Cluster
			target := text(pod, "metadata", "name")
			valueObject(claim, "metadata", "labels")["cnpg.io/instanceName"] = target
			valueObject(claim, "metadata")["annotations"] = object{"cnpg.io/pvcStatus": "ready", "preserve": "existing"}
			witness, err := bindUmamiStandbyCutover(observation, observation.Pods, target, claim, pv, volume, pdb)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "replacement UID":
				valueObject(claim, "metadata")["uid"] = "ac005841-6666-4666-8666-666666666666"
			case "became primary":
				valueObject(cluster, "status")["currentPrimary"] = target
			case "extra owner":
				refs := value(claim, "metadata", "ownerReferences").([]any)
				valueObject(claim, "metadata")["ownerReferences"] = append(refs, object{"kind": "Other"})
			case "wrong instance label":
				valueObject(claim, "metadata", "labels")["cnpg.io/instanceName"] = "umami-db-3"
			case "unready claim":
				valueObject(claim, "metadata", "annotations")["cnpg.io/pvcStatus"] = "initializing"
			case "lost Retain":
				valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			}
			changes, err := umamiStandbyClaimRetentionChanges(witness, umamiStandbyAdmission{cluster: cluster, pod: pod, claim: claim, pv: pv, volume: volume})
			if fault != "" {
				if err == nil || len(changes) != 0 {
					t.Fatal("changed original storage accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(append(identityTests(claim), changes...))
			if err != nil {
				t.Fatal(err)
			}
			patch, err := jsonpatch.DecodePatch(data)
			if err != nil {
				t.Fatal(err)
			}
			original, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			patched, err := patch.Apply(original)
			var retained object
			if err != nil || json.Unmarshal(patched, &retained) != nil {
				t.Fatal("retention patch did not apply")
			}
			valueObject(retained, "metadata")["resourceVersion"] = "12"
			if benchmarkIdentity(retained) != witness.Claim || !reflect.DeepEqual(value(retained, "spec"), value(claim, "spec")) ||
				!reflect.DeepEqual(value(retained, "metadata", "labels"), value(claim, "metadata", "labels")) ||
				text(retained, "metadata", "annotations", "cnpg.io/pvcStatus") != "detached" ||
				text(retained, "metadata", "annotations", "preserve") != "existing" ||
				!reflect.DeepEqual(value(retained, "metadata", "ownerReferences"), []any{}) {
				t.Fatal("retention patch changed storage or left CNPG ownership")
			}
			if err := verifyUmamiStandbyClaimRetention(claim, retained); err != nil {
				t.Fatal(err)
			}
			valueObject(retained, "spec")["volumeName"] = "foreign"
			if verifyUmamiStandbyClaimRetention(claim, retained) == nil {
				t.Fatal("retention readback accepted a different backing volume")
			}
		})
	}
}

func TestUmamiStandbyDetachedKeepsOriginalVolumeBoundAndUnused(t *testing.T) {
	for _, fault := range []string{"", "unretained", "claim deletion", "faulted", "unknown pods", "same pod name", "foreign consumer", "CSI attachment"} {
		t.Run(fault, func(t *testing.T) {
			observation, pod, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
			target := text(pod, "metadata", "name")
			witness, err := bindUmamiStandbyCutover(observation, observation.Pods, target, claim, pv, volume, pdb)
			if err != nil {
				t.Fatal(err)
			}
			delete(valueObject(claim, "metadata"), "ownerReferences")
			valueObject(claim, "metadata")["annotations"] = object{"cnpg.io/pvcStatus": "detached"}
			valueObject(claim, "metadata", "labels")["cnpg.io/instanceName"] = target
			status := valueObject(volume, "status")
			status["state"], status["robustness"], status["currentNodeID"] = "detached", "unknown", ""
			pods, attachments := []object{}, []object{}
			switch fault {
			case "unretained":
				valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			case "claim deletion":
				valueObject(claim, "metadata")["deletionTimestamp"] = "2026-11-27T05:00:00Z"
			case "faulted":
				status["robustness"] = "faulted"
			case "unknown pods":
				pods = nil
			case "same pod name":
				valueObject(pod, "metadata")["uid"] = "new"
				pods = []object{pod}
			case "foreign consumer":
				valueObject(pod, "metadata")["name"] = "foreign"
				pods = []object{pod}
			case "CSI attachment":
				attachments = []object{{"spec": object{"source": object{"persistentVolumeName": witness.PV.Name}}}}
			}
			if err := verifyUmamiStandbyDetached(witness, claim, pv, volume, pods, attachments); (err == nil) != (fault == "") {
				t.Fatalf("retained original attachment result: %v", err)
			}
		})
	}
}
