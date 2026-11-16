package main

import (
	"strings"
	"testing"
	"time"
)

func TestRetirementInstancesRequireSealedRecordedManager(t *testing.T) {
	for _, fault := range []string{"", "native stopped empty manager", "foreign engine manager", "foreign replica manager", "foreign frontend manager", "manager on another node", "missing runtime", "incomplete runtime"} {
		t.Run(fault, func(t *testing.T) {
			record, groups := retirementInstanceFixture(t)
			record.Version = 3
			oldNamespace := record.Namespace
			record.Namespace = benchmarkNamespace
			for kind, identity := range record.Resources {
				if kind == "Namespace" {
					identity.Name = record.Namespace
				} else if identity.Namespace == oldNamespace {
					identity.Namespace = record.Namespace
				}
				record.Resources[kind] = identity
			}
			record.V1Runtime = &benchmarkV1Runtime{Image: testRetirementImage, SettingUID: "setting-uid", SettingVersion: "17"}
			for _, index := range []string{"1", "2", "3"} {
				record.V1Runtime.Epochs = append(record.V1Runtime.Epochs, benchmarkManagerEpoch{
					Pod: "instance-manager-" + index, PodUID: "pod-" + index, ManagerUID: "manager-" + index,
					Node: "prod-worker-" + index, ContainerID: "containerd://" + strings.Repeat(index, 64),
					StartedAt: time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC),
				})
			}
			for _, kind := range []string{"engines", "replicas", "enginefrontends"} {
				groups[kind][0]["status"].(object)["instanceManagerName"] = "instance-manager-1"
			}
			if err := verifyRetirementReceipts(record); err != nil {
				t.Fatalf("invalid original-resource fixture: %v", err)
			}
			if err := validateBenchmarkRuntimeReceipt(record); err != nil {
				t.Fatalf("invalid approved runtime fixture: %v", err)
			}
			switch fault {
			case "native stopped empty manager":
				for _, kind := range []string{"engines", "replicas", "enginefrontends"} {
					delete(groups[kind][0]["status"].(object), "instanceManagerName")
				}
			case "foreign engine manager":
				groups["engines"][0]["status"].(object)["instanceManagerName"] = "instance-manager-excluded"
			case "foreign replica manager":
				groups["replicas"][0]["status"].(object)["instanceManagerName"] = "instance-manager-excluded"
			case "foreign frontend manager":
				groups["enginefrontends"][0]["status"].(object)["instanceManagerName"] = "instance-manager-excluded"
			case "manager on another node":
				groups["replicas"][0]["status"].(object)["instanceManagerName"] = "instance-manager-2"
			case "missing runtime":
				record.V1Runtime = nil
			case "incomplete runtime":
				record.V1Runtime.Epochs = record.V1Runtime.Epochs[:2]
			}
			children, err := qualifyRetirementInstances(record, groups)
			if fault == "" || fault == "native stopped empty manager" {
				if err != nil || len(children) != 4 {
					t.Fatalf("original stopped children with approved runtime refused: %v", err)
				}
			} else if err == nil {
				t.Fatal("an instance without its sealed runtime baseline qualified for retirement")
			}
		})
	}
}

func retirementInstanceFixture(t *testing.T) (benchmarkRecord, map[string][]object) {
	t.Helper()
	record := retirementFenceFixture(t)
	groups := map[string][]object{"snapshots": {}}
	for _, kind := range []string{"engines", "replicas", "enginefrontends", "volumeattachments"} {
		child := resource("scratch-" + kind)
		child["metadata"].(object)["namespace"] = "longhorn-system"
		child["metadata"].(object)["labels"] = object{"longhornvolume": "scratch-backend"}
		child["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": "scratch-backend", "uid": record.Resources["LonghornVolume"].UID}}
		child["spec"] = object{"volumeName": "scratch-backend", "dataEngine": "v1", "desireState": "stopped", "nodeID": "prod-worker-1"}
		child["status"] = object{"ownerID": "prod-worker-1", "instanceManagerName": "scratch-im", "currentState": "stopped", "starting": false, "started": false}
		if kind == "replicas" || kind == "enginefrontends" {
			child["spec"].(object)["engineName"] = "scratch-engines"
		}
		if kind == "volumeattachments" {
			child["metadata"].(object)["name"] = "scratch-backend"
			child["spec"] = object{"volume": "scratch-backend", "attachmentTickets": object{}}
			child["status"] = object{"attachmentTicketStatuses": object{}}
		}
		groups[kind] = []object{child}
	}
	return record, groups
}

func TestNoStartFenceRequiresQualifiedChildrenAndFreshCheckpoint(t *testing.T) {
	record, groups := retirementInstanceFixture(t)
	epoch := "5678-" + strings.Repeat("a", 32)
	if _, err := benchmarkRetirementInstanceFence(record, groups, epoch); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"", "5678", "foreign", epoch + "' || true"} {
		if _, err := benchmarkRetirementInstanceFence(record, groups, wrong); err == nil {
			t.Fatal("invalid checkpoint became a policy")
		}
	}
	groups["engines"][0]["status"].(object)["starting"] = true
	if _, err := benchmarkRetirementInstanceFence(record, groups, epoch); err == nil {
		t.Fatal("active instance became a no-start checkpoint")
	}
}

func TestRetirementInstancesRequireOriginalVolumeOwnerAndStoppedState(t *testing.T) {
	record, groups := retirementInstanceFixture(t)
	children, err := qualifyRetirementInstances(record, groups)
	if err != nil || len(children) != 4 {
		t.Fatalf("native stopped owner chain refused: %v", err)
	}
	for _, fault := range []string{"missing inventory", "missing owner", "foreign owner UID", "foreign owner kind", "foreign owner name", "controller-owner mismatch", "spec owner mismatch", "selector label mismatch", "foreign reserved attachment", "native snapshot dependency", "wrong engine", "unknown engine link", "starting", "started", "empty current state", "running desire", "unknown owner process", "linked clone", "pending attachment", "stale attachment status", "duplicate UID"} {
		t.Run(fault, func(t *testing.T) {
			r, g := retirementInstanceFixture(t)
			engine := g["engines"][0]
			replica := g["replicas"][0]
			attachment := g["volumeattachments"][0]
			owner := value(engine, "metadata", "ownerReferences").([]any)[0].(object)
			switch fault {
			case "missing inventory":
				delete(g, "enginefrontends")
			case "missing owner":
				delete(engine["metadata"].(object), "ownerReferences")
			case "foreign owner UID":
				owner["uid"] = "foreign"
			case "foreign owner kind":
				owner["kind"] = "Engine"
			case "foreign owner name":
				owner["name"] = "foreign"
			case "controller-owner mismatch":
				owner["controller"] = true
			case "spec owner mismatch":
				engine["spec"].(object)["volumeName"] = "foreign"
			case "selector label mismatch":
				engine["metadata"].(object)["labels"] = object{"longhornvolume": "foreign"}
			case "foreign reserved attachment":
				attachment["metadata"].(object)["labels"] = object{"longhornvolume": "foreign"}
				delete(attachment["metadata"].(object), "ownerReferences")
				attachment["spec"].(object)["volume"] = "foreign"
			case "native snapshot dependency":
				snapshot := resource("foreign-snapshot")
				snapshot["metadata"].(object)["namespace"] = "longhorn-system"
				snapshot["metadata"].(object)["labels"] = object{"longhornvolume": r.Resources["LonghornVolume"].Name}
				snapshot["spec"] = object{"volume": "foreign"}
				g["snapshots"] = []object{snapshot}
			case "wrong engine":
				replica["spec"].(object)["dataEngine"] = "v2"
			case "unknown engine link":
				replica["spec"].(object)["engineName"] = "foreign"
			case "starting":
				engine["status"].(object)["starting"] = true
			case "started":
				engine["status"].(object)["started"] = true
			case "empty current state":
				engine["status"].(object)["currentState"] = ""
			case "running desire":
				engine["spec"].(object)["desireState"] = "running"
			case "unknown owner process":
				delete(engine["status"].(object), "ownerID")
			case "linked clone":
				replica["spec"].(object)["linkedCloneSrcReplicaName"] = "foreign"
			case "pending attachment":
				attachment["spec"].(object)["attachmentTickets"] = object{"pending": object{}}
			case "stale attachment status":
				attachment["status"].(object)["attachmentTicketStatuses"] = object{"pending": object{"satisfied": false}}
			case "duplicate UID":
				replica["metadata"].(object)["uid"] = text(engine, "metadata", "uid")
			}
			if _, err := qualifyRetirementInstances(r, g); err == nil {
				t.Fatal("unproven ownership or quiescence accepted")
			}
		})
	}
}
