package main

import "testing"

func TestUmamiV2StandbyBindsLivePhysicalReplayAndCopies(t *testing.T) {
	const standby = "umami-db-4"
	storage := newRecoveredStorageFixture(t)
	for _, fault := range []string{"", "old engine", "incomplete copies", "behind", "primary", "wrong mount"} {
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
			case "old engine":
				valueObject(f.engine, "spec")["dataEngine"] = "v1"
			case "incomplete copies":
				f.replicas = f.replicas[:2]
			case "behind":
				state := obs.States[standby]
				state.ReplayLSN = "2/FF"
				obs.States[standby] = state
			case "primary":
				valueObject(obs.Cluster, "status")["currentPrimary"] = standby
			case "wrong mount":
				value(pod, "spec", "volumes").([]any)[0].(object)["persistentVolumeClaim"] = object{"claimName": "another-claim"}
			}
			got, err := bindUmamiV2Standby(obs, f.claim, f.pv, f.volume, f.engine, f.replicas, f.p.record.Storage.Disks)
			if fault == "" {
				if err != nil || got.Pod != benchmarkIdentity(pod) || got.Claim != benchmarkIdentity(f.claim) || len(got.Replicas) != 3 {
					t.Fatalf("V2 standby did not qualify: %+v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("incomplete V2 standby evidence passed: %+v", got)
			}
		})
	}
}
