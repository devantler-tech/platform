package main

import "testing"

// TestCNPGObservationHasItsOwnFixedTargetAndConfirmation tests the dispatch boundary.
func TestCNPGObservationHasItsOwnFixedTargetAndConfirmation(t *testing.T) {
	for _, source := range cnpgSources() {
		env := dispatchFixture()
		env["LONGHORN_V2_OPERATION"] = "observe-database"
		env["LONGHORN_V2_DATABASE"] = source.Name
		env["LONGHORN_V2_NODE"] = ""
		env["LONGHORN_V2_CONFIRM"] = "observe-longhorn-v2-database-" + source.Name
		getenv := func(key string) string { return env[key] }
		if err := authorizeCNPGObservation(getenv); err != nil {
			t.Fatal(err)
		}
		if authorizeRefresh(getenv) == nil || authorize(getenv, "prod-worker-1") == nil {
			t.Fatal("database observation authorized a storage operation")
		}
		for key, bad := range map[string]string{
			"GITHUB_ACTIONS": "false", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/candidate", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_REPOSITORY": "other/platform", "GITHUB_WORKFLOW_REF": "other", "GITHUB_SHA": "", "GITHUB_RUN_ID": "", "RUNNER_ENVIRONMENT": "self-hosted", "LONGHORN_V2_OPERATION": "prepare-worker", "LONGHORN_V2_DATABASE": "other", "LONGHORN_V2_NODE": "prod-worker-1", "LONGHORN_V2_CONFIRM": "benchmark-longhorn-storage",
		} {
			old := env[key]
			env[key] = bad
			if authorizeCNPGObservation(getenv) == nil {
				t.Fatalf("database observation accepted invalid %s", key)
			}
			env[key] = old
		}
	}
}
