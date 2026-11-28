package main

import "reflect"

// OpenBao uses OnDelete. Its update revision is authoritative even while
// currentRevision refers to an older revision.
func openBaoDisruptionBudget(current openBaoPairObservation, healthy int) bool {
	stateful, budget := current.stateful, current.budget
	labels := object{"app.kubernetes.io/instance": "openbao", "app.kubernetes.io/name": "openbao", "component": "server"}
	selector := object{"matchLabels": labels}
	budgetGeneration, _ := value(budget, "metadata", "generation").(float64)
	statefulGeneration, _ := value(stateful, "metadata", "generation").(float64)
	if healthy < 2 || healthy > 3 ||
		!recoveredNativeObject(stateful, "apps/v1", "StatefulSet", openBaoNamespace) ||
		text(stateful, "metadata", "name") != openBaoNamespace || statefulGeneration < 1 ||
		value(stateful, "status", "observedGeneration") != statefulGeneration ||
		!reflect.DeepEqual(value(stateful, "spec", "selector"), selector) ||
		text(stateful, "spec", "updateStrategy", "type") != "OnDelete" ||
		text(stateful, "spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted") != "Retain" ||
		text(stateful, "spec", "persistentVolumeClaimRetentionPolicy", "whenScaled") != "Retain" ||
		value(stateful, "spec", "replicas") != float64(3) ||
		value(stateful, "status", "readyReplicas") != float64(healthy) ||
		value(stateful, "status", "updatedReplicas") != float64(healthy) ||
		!recoveredNativeObject(budget, "policy/v1", "PodDisruptionBudget", openBaoNamespace) ||
		text(budget, "metadata", "name") != openBaoNamespace || budgetGeneration < 1 ||
		value(budget, "status", "observedGeneration") != budgetGeneration ||
		!reflect.DeepEqual(value(budget, "spec", "selector"), selector) || value(budget, "spec", "maxUnavailable") != float64(1) ||
		value(budget, "status", "currentHealthy") != float64(healthy) || value(budget, "status", "expectedPods") != float64(3) ||
		value(budget, "status", "desiredHealthy") != float64(2) || value(budget, "status", "disruptionsAllowed") != float64(healthy-2) ||
		healthy == 3 && !absentOrEmptyMap(value(budget, "status", "disruptedPods")) {
		return false
	}
	selected := 0
	for _, pod := range current.pods {
		matches := true
		for name, expected := range labels {
			matches = matches && value(pod, "metadata", "labels", name) == expected
		}
		if matches {
			if !knownOpenBaoMember(text(pod, "metadata", "name")) || !openBaoOriginalStorageMember(pod, stateful) {
				return false
			}
			selected++
		}
	}
	return selected == healthy
}
