package main

import (
	"reflect"
	"testing"
	"time"
)

func TestUmamiSwitchoverChangesOnlyTheWitnessedCNPGStatus(t *testing.T) {
	for _, fault := range []string{"", "pending promotion", "stale readiness", "wrong owner", "same target", "fenced pod"} {
		t.Run(fault, func(t *testing.T) {
			const old, target = "umami-db-1", "umami-db-9"
			uid := "c6339ec2-6239-4e3f-85fd-fa778d6d761d"
			fenced := ""
			if fault == "fenced pod" {
				fenced = `["umami-db-9"]`
			}
			picked := target
			if fault == "same target" {
				picked = old
			}
			ready := object{"type": "Ready", "status": "True", "reason": "ClusterIsReady", "lastTransitionTime": "2026-11-27T06:00:00Z"}
			other := object{"type": "ContinuousArchiving", "status": "True", "reason": "ContinuousArchivingSuccess"}
			status := object{"currentPrimary": old, "targetPrimary": old, "phase": "Cluster in healthy state",
				"targetPrimaryTimestamp": "2026-11-27T06:00:00Z", "phaseReason": "", "conditions": []any{ready, other}}
			cluster := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": object{
				"uid": uid, "name": "umami-db", "namespace": "umami", "resourceVersion": "912",
				"annotations": object{"cnpg.io/fencedInstances": fenced}}, "status": status}
			switch fault {
			case "pending promotion":
				status["targetPrimary"] = "umami-db-7"
			case "stale readiness":
				ready["status"] = "False"
			case "wrong owner":
				valueObject(cluster, "metadata")["namespace"] = "other"
			}
			now := time.Date(2026, 11, 27, 6, 3, 9, 0, time.UTC)
			patch, err := umamiV2SwitchoverPatch(cluster, old, picked, now)
			if fault != "" {
				if err == nil || len(patch) != 0 {
					t.Fatal("unsafe promotion admitted")
				}
				return
			}
			if err != nil || len(patch) != 7 {
				t.Fatalf("qualified promotion refused: %v", err)
			}
			if !reflect.DeepEqual(patch[0], object{"op": "test", "path": "/metadata/uid", "value": uid}) ||
				!reflect.DeepEqual(patch[1], object{"op": "test", "path": "/metadata/resourceVersion", "value": "912"}) {
				t.Fatal("the write is unbound from its observation")
			}
			for _, mutation := range patch[2:] {
				switch text(mutation, "path") {
				case "/status/targetPrimary", "/status/targetPrimaryTimestamp", "/status/phase", "/status/phaseReason":
				case "/status/conditions":
					conditions := value(mutation, "value").([]any)
					if !reflect.DeepEqual(conditions[1], other) || text(conditions[0].(object), "status") != "False" || ready["status"] != "True" {
						t.Fatal("unrelated conditions or original snapshot were changed")
					}
				default:
					t.Fatalf("unreviewed switchover field %s", text(mutation, "path"))
				}
			}
		})
	}
}

func TestUmamiSwitchoverRequiresAnAdvancedTimelineWithoutLosingTheOldBarrier(t *testing.T) {
	for _, fault := range []string{"", "same timeline", "lost WAL", "another cluster", "old primary still serving", "standby not replaying"} {
		t.Run(fault, func(t *testing.T) {
			const old, target = "umami-db-1", "umami-db-9"
			previous := postgresState{SystemID: "7560127483245078101", Timeline: "7", FlushLSN: "0/400"}
			primary := postgresState{SystemID: previous.SystemID, Timeline: "8", FlushLSN: "0/410"}
			standby := postgresState{SystemID: previous.SystemID, Timeline: "8", ReplayLSN: "0/420", InRecovery: true}
			status := object{"currentPrimary": target, "targetPrimary": target}
			switch fault {
			case "same timeline":
				primary.Timeline, standby.Timeline = previous.Timeline, previous.Timeline
			case "lost WAL":
				primary.FlushLSN = "0/3FF"
			case "another cluster":
				primary.SystemID = "11"
			case "old primary still serving":
				status["currentPrimary"] = old
			case "standby not replaying":
				standby.ReplayLSN = "0/40F"
			}
			after := cnpgObservation{Cluster: object{"status": status}, States: map[string]postgresState{
				old: standby, target: primary, "umami-db-2": standby, "umami-db-3": standby,
			}}
			if err := verifyUmamiPromotionHistory(previous, after, target); (err == nil) != (fault == "") {
				t.Fatalf("unexpected promotion readback: %v", err)
			}
		})
	}
}
