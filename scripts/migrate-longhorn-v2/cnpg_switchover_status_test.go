package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestUmamiSwitchoverReadbackBindsTheRecordedPromotion(t *testing.T) {
	const old, target = "umami-db-11", "umami-db-12"
	for _, fault := range []string{"", "wrong owner", "different target", "wrong ordinal", "changed record"} {
		t.Run(fault, func(t *testing.T) {
			record := umamiV2PromotionRecord{
				Source: "45f385ab8aaff40c04aaa2b6c0bba23ac55706d4", Run: "42", Target: target,
				Witness: umamiV2Promotion{
					Database:   benchmarkResource{Name: "umami-db", Namespace: "umami"},
					Previous:   benchmarkResource{Name: old, Namespace: "umami"},
					Generation: 17, PreviousTargetSince: "2026-11-27T06:00:00Z",
					Standbys: map[string]cnpgRecoveredStorage{
						target:       {Pod: benchmarkResource{Name: target, Namespace: "umami"}},
						"umami-db-2": {Pod: benchmarkResource{Name: "umami-db-2", Namespace: "umami"}},
						"umami-db-3": {Pod: benchmarkResource{Name: "umami-db-3", Namespace: "umami"}},
					},
					PreviousState: postgresState{SystemID: "7560127483245078101", Timeline: "7", FlushLSN: "0/400"},
				},
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			owner := record.Run
			requestedPrevious, requestedTarget := old, target
			switch fault {
			case "wrong owner":
				owner = "43"
			case "different target":
				requestedTarget = "umami-db-2"
			case "wrong ordinal":
				requestedPrevious = "umami-db-3"
			case "changed record":
				data = append(data, ' ')
			}
			journal := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{
				"name": umamiV2SwitchoverJournal, "namespace": umamiV2SwitchoverNamespace, "resourceVersion": "12",
				"uid":    "c6339ec2-6239-4e3f-85fd-fa778d6d761d",
				"labels": object{"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": owner}},
				"data": object{"record": string(data)}}
			if _, err := bindUmamiPromotionRecord(journal, requestedPrevious, requestedTarget); (err == nil) != (fault == "") {
				t.Fatalf("unexpected journal readback: %v", err)
			}
		})
	}
}

func TestUmamiSwitchoverRetryRequiresAnUntouchedWriter(t *testing.T) {
	for _, fault := range []string{"", "new writer requested", "controller rolled back", "new spec", "old writer restarted", "lost WAL"} {
		t.Run(fault, func(t *testing.T) {
			obs := umamiV2TransitionObservation(t)
			status := valueObject(obs.Cluster, "status")
			const since = "2026-11-27T06:00:00Z"
			status["targetPrimaryTimestamp"] = since
			old := text(obs.Cluster, "status", "currentPrimary")
			generation, ok := nativeObservationUint(value(obs.Cluster, "metadata", "generation"))
			if !ok || generation == 0 {
				t.Fatal("invalid observation fixture generation")
			}
			witness := umamiV2Promotion{Database: benchmarkIdentity(obs.Cluster), Generation: generation, PreviousTargetSince: since,
				Previous: benchmarkResource{Name: old}, PreviousState: postgresState{
					SystemID: "7560127483245078101", Timeline: "7", FlushLSN: "0/400", PostmasterStart: "2026-11-27 06:00:00+00",
				}}
			latest := witness
			latest.PreviousState.FlushLSN = "0/480"
			switch fault {
			case "new writer requested":
				status["targetPrimary"] = "umami-db-4"
			case "controller rolled back":
				status["targetPrimaryTimestamp"] = "2026-11-27T06:00:01Z"
			case "new spec":
				valueObject(obs.Cluster, "metadata")["generation"] = float64(generation + 1)
			case "old writer restarted":
				latest.PreviousState.PostmasterStart = "2026-11-27 06:00:01+00"
			case "lost WAL":
				latest.PreviousState.FlushLSN = "0/3F0"
			}
			allowed := umamiOriginalWriterCurrent(obs.Cluster, witness) && sameUmamiPromotionWitness(witness, latest)
			if allowed != (fault == "") {
				t.Fatal("retry did not protect the original control state and WAL barrier")
			}
		})
	}
}

func TestUmamiSwitchoverWriterRouteRejectsTheOldWriter(t *testing.T) {
	const serviceUID = "c6339ec2-6239-4e3f-85fd-fa778d6d761d"
	const writerUID = "4d48bfc9-7352-4fc4-a982-086078e144c9"
	const writerName = "umami-db-12"
	for _, fault := range []string{"", "stale writer", "stale slice owner", "draining endpoint", "missing route", "both writers", "wrong address", "wrong port"} {
		t.Run(fault, func(t *testing.T) {
			controller := object{"apiVersion": "v1", "kind": "Service", "name": "umami-db-rw", "uid": serviceUID, "controller": true}
			target := object{"kind": "Pod", "name": writerName, "uid": writerUID, "namespace": "umami"}
			state := object{"ready": true, "serving": true, "terminating": false}
			addr := []any{"10.0.0.41"}
			slicePort := object{"name": "postgres", "protocol": "TCP", "port": float64(5432)}
			endpoints := []any{object{"conditions": state, "targetRef": target, "addresses": addr}}
			switch fault {
			case "stale writer":
				target["uid"] = "1aa817ea-b284-4aa9-a9b8-cbcadf4c9c2e"
			case "stale slice owner":
				controller["uid"] = "022b6c1a-129a-4d25-a0ac-aa434b186c1b"
			case "draining endpoint":
				state["terminating"] = true
			case "missing route":
				endpoints = []any{}
			case "both writers":
				endpoints = append(endpoints, object{"conditions": state, "targetRef": object{
					"kind": "Pod", "namespace": "umami", "name": "umami-db-11", "uid": "1aa817ea-b284-4aa9-a9b8-cbcadf4c9c2e"}})
			case "wrong address":
				addr[0] = "10.0.0.42"
			case "wrong port":
				slicePort["port"] = float64(5433)
			}
			slice := object{"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice", "metadata": object{
				"name": "umami-db-rw-q8kvt", "namespace": "umami", "uid": "5010f302-ded4-48cb-8ce3-2dcfdf37055b", "resourceVersion": "17",
				"labels": object{"kubernetes.io/service-name": "umami-db-rw"}, "ownerReferences": []any{controller}}, "endpoints": endpoints,
				"addressType": "IPv4", "ports": []any{slicePort}}
			service := object{"metadata": object{"uid": serviceUID}, "spec": object{"ipFamilies": []any{"IPv4"},
				"ports": []any{object{"name": "postgres", "protocol": "TCP", "port": float64(5432), "targetPort": float64(5432)}}}}
			writer := object{"metadata": object{"name": writerName, "uid": writerUID}, "status": object{"podIPs": []any{object{"ip": "10.0.0.41"}}}}
			if err := bindUmamiWriterEndpoints(service, writer, []object{slice}); (err == nil) != (fault == "") {
				t.Fatalf("unexpected writer route proof: %v", err)
			}
		})
	}
}

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

func TestUmamiSwitchoverRequiresTheOriginalTimelineParentAndWALFork(t *testing.T) {
	previous := postgresState{SystemID: "7560127483245078101", Timeline: "7", FlushLSN: "0/430"}
	promoted := postgresState{SystemID: previous.SystemID, Timeline: "9", FlushLSN: "0/500"}
	for _, input := range []struct {
		name, history string
		want          bool
	}{
		{"native ancestry", "1\t0/100\tno recovery target specified\n7\t0/440\tno recovery target specified\n", true},
		{"fork before witnessed flush", "1\t0/100\trecovery\n7\t0/400\trecovery\n", false},
		{"unrelated branch despite higher WAL", "1\t0/100\trecovery\n8\t0/440\trecovery\n", false},
		{"unwitnessed fork beyond new flush", "1\t0/100\trecovery\n7\t0/510\trecovery\n", false},
		{"malformed history", "1\t0/100\trecovery\n7\t0/440", false},
		{"missing history", "", false},
	} {
		t.Run(input.name, func(t *testing.T) {
			if err := verifyUmamiTimelineParent(input.history, previous, promoted); (err == nil) != input.want {
				t.Fatalf("unexpected timeline ancestry decision: %v", err)
			}
		})
	}
}
