package main

import "testing"

func TestWeddingV2TransitionBindsExactArchiveDatabaseAndSpare(t *testing.T) {
	for _, fault := range []string{"", "foreign namespace", "foreign cluster", "no spare", "in-use resize", "wrong size", "wrong storage class", "separate WAL", "tablespace"} {
		t.Run(fault, func(t *testing.T) {
			meta := object{"name": "wedding-db", "namespace": "wedding-app"}
			storage := object{"storageClass": "longhorn-v2", "size": "2Gi", "resizeInUseVolumes": false}
			spec := object{"instances": float64(4), "storage": storage}
			cluster := object{"metadata": meta, "spec": spec}
			switch fault {
			case "foreign namespace":
				meta["namespace"] = "umami"
			case "foreign cluster":
				meta["name"] = "customer-identity-db"
			case "no spare":
				spec["instances"] = float64(3)
			case "in-use resize":
				storage["resizeInUseVolumes"] = true
			case "wrong size":
				storage["size"] = "5Gi"
			case "wrong storage class":
				storage["storageClass"] = "longhorn-wffc"
			case "separate WAL":
				spec["walStorage"] = object{"size": "1Gi"}
			case "tablespace":
				spec["tablespaces"] = []any{object{"name": "other"}}
			}
			profile, accepted := cnpgStandbyTransition(cluster)
			if accepted != (fault == "") {
				t.Fatalf("transition accepted=%t", accepted)
			}
			if accepted && (profile.Source != (cnpgSource{Namespace: "wedding-app", Name: "wedding-db"}) || profile.Instances != 4 || profile.Size != "2Gi" || profile.SizeBytes != "2147483648") {
				t.Fatalf("wrong admitted transition profile: %+v", profile)
			}
		})
	}
}

func TestWeddingStandbyRequiresCurrentOperatorDisruptionBudget(t *testing.T) {
	for _, fault := range []string{"", "only two healthy", "weak threshold", "foreign owner", "old generation", "another disruption", "selector differs"} {
		t.Run(fault, func(t *testing.T) {
			cluster := object{"metadata": object{"name": "wedding-db", "namespace": "wedding-app", "uid": freshTestUID},
				"spec": object{"instances": float64(4), "storage": object{"storageClass": "longhorn-v2", "size": "2Gi", "resizeInUseVolumes": false}}}
			metadata := object{"name": "wedding-db", "namespace": "wedding-app", "uid": "ac005841-2222-4222-8222-222222222222", "generation": float64(3), "resourceVersion": "14"}
			owner := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "controller": true, "name": "wedding-db", "uid": freshTestUID}
			metadata["ownerReferences"] = []any{owner}
			labels := object{"cnpg.io/cluster": "wedding-db", "cnpg.io/instanceRole": "replica"}
			spec := object{"minAvailable": float64(2), "selector": object{"matchLabels": labels}}
			status := object{"observedGeneration": float64(3), "disruptionsAllowed": float64(1), "desiredHealthy": float64(2), "expectedPods": float64(3), "currentHealthy": float64(3)}
			budget := object{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget", "metadata": metadata, "spec": spec, "status": status}

			switch fault {
			case "only two healthy":
				status["currentHealthy"] = float64(2)
			case "weak threshold":
				spec["minAvailable"] = float64(1)
			case "foreign owner":
				owner["uid"] = text(budget, "metadata", "uid")
			case "old generation":
				status["observedGeneration"] = float64(2)
			case "another disruption":
				status["disruptedPods"] = object{"wedding-db-3": "2026-11-27T12:00:00Z"}
			case "selector differs":
				labels["cnpg.io/instanceRole"] = "primary"
			}
			profile, ok := cnpgStandbyTransition(cluster)
			if !ok || cnpgTransitionDisruptionAllowed(budget, cluster, profile) != (fault == "") {
				t.Fatal("the budget qualification result did not match its exact owner, generation or healthy standby roster")
			}
		})
	}
}
