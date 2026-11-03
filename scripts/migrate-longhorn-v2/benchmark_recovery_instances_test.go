package main

import (
	"strings"
	"testing"
)

func retirementInstanceFixture(t *testing.T) (benchmarkRecord, map[string][]object) {
	t.Helper()
	record := retirementFenceFixture(t)
	groups := map[string][]object{}
	for _, kind := range []string{"engines", "replicas", "enginefrontends", "volumeattachments"} {
		child := resource("scratch-" + kind)
		child["metadata"].(object)["namespace"] = "longhorn-system"
		child["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": "scratch-backend", "uid": record.Resources["LonghornVolume"].UID}}
		child["spec"] = object{"volumeName": "scratch-backend", "dataEngine": "v1", "desireState": "stopped", "nodeID": "prod-worker-1"}
		child["status"] = object{"ownerID": "prod-worker-1", "instanceManagerName": "scratch-im", "currentState": "stopped", "starting": false, "started": false}
		if kind == "replicas" || kind == "enginefrontends" {
			child["spec"].(object)["engineName"] = "scratch-engines"
		}
		if kind == "volumeattachments" {
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
	for _, fault := range []string{"missing inventory", "missing owner", "foreign owner UID", "foreign owner kind", "foreign owner name", "controller-owner mismatch", "spec owner mismatch", "wrong engine", "unknown engine link", "starting", "started", "empty current state", "running desire", "unknown owner process", "linked clone", "pending attachment", "stale attachment status", "duplicate UID"} {
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
