package main

import "fmt"

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
	cs, ok := distinctConditions(owner)
	if !ok || len(cs) != 3 || cs["Ready"]["status"] != "False" || cs["Ready"]["reason"] != "SourceNotReady" || cs["Reconciling"]["status"] != "True" || cs["Reconciling"]["reason"] != "Progressing" || cs["Reconciling"]["message"] != "Fulfilling prerequisites" || cs["Released"]["status"] != "True" {
		return false
	}
	if reason := cs["Released"]["reason"]; reason != "InstallSucceeded" && reason != "UpgradeSucceeded" {
		return false
	}
	// Prerequisite conditions are refreshed before the source-wait return, but
	// Released describes the last actual action and can predate an in-sync pass.
	// The captured options patch is the only allowed one-generation observation
	// gap. Both prerequisite conditions must describe the same allowed phase.
	prerequisiteGeneration := integer(cs["Ready"]["observedGeneration"])
	if prerequisiteGeneration != observed && prerequisiteGeneration != generation || integer(cs["Reconciling"]["observedGeneration"]) != prerequisiteGeneration {
		return false
	}
	releaseGeneration := integer(cs["Released"]["observedGeneration"])
	if releaseGeneration < 1 || releaseGeneration > observed {
		return false
	}
	// A historical success is accepted only when it names the latest deployed
	// controller snapshot exactly. Stored release equality is still mandatory.
	latest, ok := latestControllerHistory(owner)
	if !ok || latest["status"] != "deployed" || text(latest["chartName"]) == "" || text(latest["chartVersion"]) == "" {
		return false
	}
	action := "upgrade"
	if cs["Released"]["reason"] == "InstallSucceeded" {
		action = "install"
	}
	message := fmt.Sprintf("Helm %s succeeded for release %s/%s.v%d with chart %s@%s", action, text(latest["namespace"]), text(latest["name"]), integer(latest["version"]), text(latest["chartName"]), text(latest["chartVersion"]))
	if cs["Released"]["message"] != message {
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
	result, ok := distinctConditions(resource)
	if !ok {
		return nil, false
	}
	for _, condition := range result {
		if integer(condition["observedGeneration"]) != generation {
			return nil, false
		}
	}
	return result, true
}

func distinctConditions(resource map[string]any) (map[string]map[string]any, bool) {
	values, ok := field(resource, "status", "conditions").([]any)
	if !ok || len(values) == 0 || len(values) > 32 {
		return nil, false
	}
	result := map[string]map[string]any{}
	for _, value := range values {
		condition := object(value)
		kind, status := text(condition["type"]), text(condition["status"])
		if kind == "" || result[kind] != nil || status != "True" && status != "False" && status != "Unknown" {
			return nil, false
		}
		result[kind] = condition
	}
	return result, true
}
