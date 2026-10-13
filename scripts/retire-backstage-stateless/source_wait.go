package main

// Flux v1.5.5 marks Reconciling before checking source readiness. Its deletion
// branch runs before source reconciliation, so this exact prerequisite wait is
// not an active Helm action. Stored/controller history and ownership are checked
// independently by the caller. See the pinned helmrelease_controller.go at
// https://github.com/fluxcd/helm-controller/blob/v1.5.5/internal/controller/helmrelease_controller.go.
func waitingForUnavailableSource(owner, chart map[string]any) bool {
	observed := integer(field(owner, "status", "observedGeneration"))
	generation := integer(field(owner, "metadata", "generation"))
	// Only the known uninstall-options patch may temporarily leave one new
	// generation unobserved; eligible requires fully current evidence beforehand.
	if observed < 1 || generation < observed || generation-observed > 1 {
		return false
	}
	cs, ok := observedConditions(owner, observed)
	if !ok || len(cs) != 3 || cs["Ready"]["status"] != "False" || cs["Ready"]["reason"] != "SourceNotReady" || cs["Reconciling"]["status"] != "True" || cs["Reconciling"]["reason"] != "Progressing" || cs["Reconciling"]["message"] != "Fulfilling prerequisites" || cs["Released"]["status"] != "True" {
		return false
	}
	if reason := cs["Released"]["reason"]; reason != "InstallSucceeded" && reason != "UpgradeSucceeded" {
		return false
	}
	chartGeneration := integer(field(chart, "metadata", "generation"))
	if !metadataValid(chart) || chartGeneration < 1 || integer(field(chart, "status", "observedGeneration")) != chartGeneration {
		return false
	}
	chartConditions, ok := observedConditions(chart, chartGeneration)
	return ok && chartConditions["Ready"]["status"] == "False" && chartConditions["Ready"]["reason"] == "SourceUnavailable"
}

// Incomplete, stale or duplicate conditions cannot identify a controller phase.
func observedConditions(resource map[string]any, generation int) (map[string]map[string]any, bool) {
	values, ok := field(resource, "status", "conditions").([]any)
	if !ok || len(values) == 0 || len(values) > 32 {
		return nil, false
	}
	result := map[string]map[string]any{}
	for _, value := range values {
		condition := object(value)
		kind, status := text(condition["type"]), text(condition["status"])
		if kind == "" || result[kind] != nil || status != "True" && status != "False" && status != "Unknown" || integer(condition["observedGeneration"]) != generation {
			return nil, false
		}
		result[kind] = condition
	}
	return result, true
}
