package main

import "testing"

func TestUmamiV2StandbyBindsLivePhysicalReplayAndCopies(t *testing.T) {
	const standby = "umami-db-4"
	storage := newRecoveredStorageFixture(t)
	for _, fault := range []string{"", "retained peer", "recovery roster", "recovery replaced peer", "unwitnessed recovery roster",
		"missing healthy peer", "missing retention witness", "retained peer behind", "old engine", "incomplete copies", "behind", "primary", "wrong mount"} {
		t.Run(fault, func(t *testing.T) {
			f := storage.copyBindingObjects(t)
			obs := umamiV2TransitionObservation(t)
			pod := obs.Pods[3]
			pod["apiVersion"], pod["kind"] = "v1", "Pod"
			valueObject(pod, "spec")["nodeName"] = text(f.engine, "spec", "nodeID")
			valueObject(pod, "spec")["volumes"] = []any{object{"name": "pgdata", "persistentVolumeClaim": object{"claimName": standby}}}
			value(pod, "spec", "containers").([]any)[0].(object)["volumeMounts"] = []any{object{"name": "pgdata", "mountPath": "/var/lib/postgresql/data"}}
			pvc := valueObject(f.claim, "metadata")
			pvc["name"], pvc["namespace"] = standby, "umami"
			pvc["labels"] = valueObject(pod, "metadata", "labels")
			pvc["ownerReferences"] = value(pod, "metadata", "ownerReferences")
			claimRef := valueObject(f.pv, "spec", "claimRef")
			claimRef["name"], claimRef["namespace"] = standby, "umami"
			volumeRef := valueObject(f.volume, "status", "kubernetesStatus")
			volumeRef["pvcName"], volumeRef["namespace"] = standby, "umami"

			switch fault {
			case "recovery roster", "recovery replaced peer", "unwitnessed recovery roster":
				obs.Pods = []object{obs.Pods[0], obs.Pods[2], obs.Pods[3]}
				status := valueObject(obs.Cluster, "status")
				status["readyInstances"] = float64(3)
				status["conditions"] = []any{object{"type": "ContinuousArchiving", "status": "True"}}
				if fault != "unwitnessed recovery roster" {
					obs.umamiRecoveryPeers = map[string]benchmarkResource{}
					for _, peer := range obs.Pods {
						identity := benchmarkIdentity(peer)
						obs.umamiRecoveryPeers[identity.Name] = identity
					}
				}
				if fault == "recovery replaced peer" {
					valueObject(obs.Pods[1], "metadata")["uid"] = "ac005841-c004-4004-8004-000000000004"
				}
			case "retained peer", "missing healthy peer", "missing retention witness", "retained peer behind":
				obs.retainedStandby = "umami-db-2"
				status := valueObject(obs.Cluster, "status")
				status["instances"] = float64(3)
				status["healthyPVC"] = []any{"umami-db-1", "umami-db-3", standby}
			}
			switch fault {
			case "missing healthy peer":
				valueObject(obs.Cluster, "status")["healthyPVC"] = []any{"umami-db-1", obs.retainedStandby, standby}
			case "missing retention witness":
				obs.retainedStandby = ""
			case "old engine":
				valueObject(f.engine, "spec")["dataEngine"] = "v1"
			case "incomplete copies":
				f.replicas = f.replicas[:2]
			case "behind", "retained peer behind":
				state := obs.States[standby]
				state.ReplayLSN = "2/FF"
				obs.States[standby] = state
			case "primary":
				valueObject(obs.Cluster, "status")["currentPrimary"] = standby
			case "wrong mount":
				value(pod, "spec", "volumes").([]any)[0].(object)["persistentVolumeClaim"] = object{"claimName": "another-claim"}
			}
			got, err := bindUmamiV2Standby(obs, f.claim, f.pv, f.volume, f.engine, f.replicas, f.p.record.Storage.Disks)
			if fault == "" || fault == "retained peer" || fault == "recovery roster" {
				if err != nil || got.Pod != benchmarkIdentity(pod) || got.Claim != benchmarkIdentity(f.claim) || len(got.Replicas) != 3 {
					t.Fatalf("V2 standby did not qualify: %+v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("incomplete V2 standby evidence passed: %+v", got)
			}
		})
	}
}
