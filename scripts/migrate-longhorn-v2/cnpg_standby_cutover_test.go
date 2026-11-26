package main

import "testing"

func TestUmamiStandbyCutoverBindsTheOriginalRetainedClaim(t *testing.T) {
	for _, fault := range []string{"", "original class still active", "missing spare", "primary", "replay behind", "another Pod", "foreign claim", "unretained", "unprotected claim", "changed volume", "no permitted disruption", "weak budget"} {
		t.Run(fault, func(t *testing.T) {
			observation := umamiV2TransitionObservation(t)
			target := "umami-db-2"
			cluster := observation.Cluster
			valueObject(cluster, "spec")["storage"] = object{"storageClass": "longhorn-v2", "resizeInUseVolumes": false, "size": "5Gi"}
			byName, err := indexed(observation.Pods)
			if err != nil {
				t.Fatal(err)
			}
			pod := byName[target]
			valueObject(pod, "spec")["nodeName"] = "prod-worker-2"
			valueObject(pod, "spec")["volumes"] = []any{object{"name": "pgdata", "persistentVolumeClaim": object{"claimName": target}}}
			write := value(pod, "spec", "containers").([]any)[0].(object)
			write["volumeMounts"] = []any{object{"name": "pgdata", "mountPath": "/var/lib/postgresql/data"}}
			own := []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "umami-db", "uid": text(cluster, "metadata", "uid"), "controller": true}}
			const claimUID = "ac005841-1111-4111-8111-111111111111"
			const pvUID = "ac005841-2222-4222-8222-222222222222"
			const volumeUID = "ac005841-3333-4333-8333-333333333333"
			pvName := "pvc-" + claimUID
			claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": object{"name": target, "uid": claimUID, "namespace": "umami", "resourceVersion": "11", "ownerReferences": own,
					"labels": object{"cnpg.io/cluster": "umami-db"}, "finalizers": []any{"kubernetes.io/pvc-protection"}},
				"spec": object{"volumeName": pvName, "storageClassName": "longhorn-wffc", "volumeMode": "Filesystem",
					"accessModes": []any{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": "5Gi"}}},
				"status": object{"phase": "Bound", "capacity": object{"storage": "5Gi"}}}
			pv := object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": object{"name": pvName, "uid": pvUID, "resourceVersion": "14"},
				"spec": object{"persistentVolumeReclaimPolicy": "Retain", "storageClassName": "longhorn-wffc", "volumeMode": "Filesystem",
					"capacity": object{"storage": "5Gi"}, "accessModes": []any{"ReadWriteOnce"},
					"claimRef": object{"name": target, "namespace": "umami", "uid": claimUID},
					"csi":      object{"driver": "driver.longhorn.io", "volumeHandle": pvName}}, "status": object{"phase": "Bound"}}
			volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
				"metadata": object{"name": pvName, "namespace": "longhorn-system", "uid": volumeUID, "resourceVersion": "8"},
				"spec":     object{"dataEngine": "v1", "size": "5368709120", "numberOfReplicas": float64(3)},
				"status": object{"state": "attached", "robustness": "healthy", "currentNodeID": text(pod, "spec", "nodeName"),
					"kubernetesStatus": object{"pvName": pvName, "pvcName": target, "namespace": "umami"}}}
			pdb := object{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
				"metadata": object{"name": "umami-db", "namespace": "umami", "uid": "ac005841-4444-4444-8444-444444444444", "resourceVersion": "50", "generation": float64(1), "ownerReferences": own},
				"spec": object{"minAvailable": float64(2),
					"selector": object{"matchLabels": object{"cnpg.io/cluster": "umami-db", "cnpg.io/instanceRole": "replica"}}},
				"status": object{"observedGeneration": float64(1), "currentHealthy": float64(3), "expectedPods": float64(3), "desiredHealthy": float64(2), "disruptionsAllowed": float64(1)}}
			pods := observation.Pods
			switch fault {
			case "original class still active":
				valueObject(cluster, "spec", "storage")["storageClass"] = "longhorn-wffc"
			case "missing spare":
				valueObject(cluster, "spec")["instances"] = float64(3)
				observation.Pods = observation.Pods[:3]
				delete(observation.States, "umami-db-4")
			case "primary":
				target = "umami-db-1"
			case "replay behind":
				state := observation.States[target]
				state.ReplayLSN = "0/0"
				observation.States[target] = state
			case "another Pod":
				pods = append(pods, object{"metadata": object{"namespace": "umami", "name": "foreign"}, "spec": valueObject(pod, "spec")})
			case "foreign claim":
				valueObject(pv, "spec", "claimRef")["uid"] = volumeUID
			case "unretained":
				valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			case "unprotected claim":
				delete(valueObject(claim, "metadata"), "finalizers")
			case "changed volume":
				valueObject(volume, "spec")["dataEngine"] = "v2"
			case "no permitted disruption":
				valueObject(pdb, "status")["disruptionsAllowed"] = float64(0)
			case "weak budget":
				valueObject(pdb, "spec")["minAvailable"] = float64(1)
			}
			result, err := bindUmamiStandbyCutover(observation, pods, target, claim, pv, volume, pdb)
			if (err == nil) != (fault == "") || err == nil && (result.Claim.UID != claimUID || result.PV.UID != pvUID || result.Volume.UID != volumeUID) {
				t.Fatalf("unexpected standby qualification: %+v %v", result, err)
			}
		})
	}
}
