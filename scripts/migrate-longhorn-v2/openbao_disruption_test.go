package main

import (
	"strconv"
	"testing"
)

func TestOpenBaoDisruptionBudgetTracksCurrentOnDeleteReplicas(t *testing.T) {
	for _, healthy := range []int{2, 3} {
		t.Run(strconv.Itoa(healthy), func(t *testing.T) {
			stateful, _, _, _, _ := openBaoProviderStorageFixture()
			labels := object{"app.kubernetes.io/instance": "openbao", "app.kubernetes.io/name": "openbao", "component": "server"}
			selector := object{"matchLabels": labels}
			spec, status := valueObject(stateful, "spec"), valueObject(stateful, "status")
			spec["selector"] = selector
			spec["persistentVolumeClaimRetentionPolicy"] = object{"whenDeleted": "Retain", "whenScaled": "Retain"}
			status["currentRevision"] = "older-revision"
			status["readyReplicas"], status["updatedReplicas"] = float64(healthy), float64(healthy)
			budget := object{"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
				"metadata": object{"name": "openbao", "namespace": "openbao", "resourceVersion": "21",
					"uid": "9ff20f85-0e17-421e-8e89-5be7e84be502", "generation": float64(1)},
				"spec": object{"selector": selector, "maxUnavailable": float64(1)},
				"status": object{"observedGeneration": float64(1), "currentHealthy": float64(healthy), "desiredHealthy": float64(2),
					"expectedPods": float64(3), "disruptionsAllowed": float64(healthy - 2)}}
			current := openBaoPairObservation{stateful: stateful, budget: budget}
			for ordinal := range healthy {
				_, pods, _, _, _ := openBaoProviderStorageFixture()
				pod := pods[0]
				valueObject(pod, "metadata")["name"] = "openbao-" + strconv.Itoa(ordinal)
				for key, label := range labels {
					valueObject(pod, "metadata", "labels")[key] = label
				}
				current.pods = append(current.pods, pod)
			}
			if !openBaoDisruptionBudget(current, healthy) {
				t.Fatal("the current OnDelete roster was rejected")
			}
			valueObject(budget, "status")["observedGeneration"] = float64(0)
			if openBaoDisruptionBudget(current, healthy) {
				t.Fatal("a stale budget was accepted")
			}
			valueObject(budget, "status")["observedGeneration"] = float64(1)
			valueObject(current.pods[0], "metadata", "labels")["controller-revision-hash"] = "older-revision"
			if openBaoDisruptionBudget(current, healthy) {
				t.Fatal("a member on the older revision was accepted")
			}
		})
	}
}
