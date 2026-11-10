package main

import "testing"

func TestUnusedTemporaryPreparationRequiresItsSeparateExactDispatch(t *testing.T) {
	for _, selection := range []string{"temporary-v1-1", "temporary-v1-2"} {
		env := dispatchFixture()
		env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = "prepare-unused-temporary-worker", selection
		env["LONGHORN_V2_CONFIRM"] = "prepare-unused-longhorn-v2-" + selection
		getenv := func(key string) string { return env[key] }
		if err := authorizeTemporaryPreparation(getenv, selection); err != nil {
			t.Fatal("confirmed unused temporary preparation was rejected:", err)
		}
		if authorize(getenv, selection) == nil || authorizeInventory(getenv, selection) == nil || authorizeRefresh(getenv) == nil || authorizeBenchmark(getenv, "v1") == nil {
			t.Fatal("temporary preparation authorized a different workflow operation")
		}
		for key, bad := range map[string]string{
			"LONGHORN_V2_NODE": "prod-worker-1", "LONGHORN_V2_CONFIRM": "prepare-longhorn-v2-prod-worker-1",
			"LONGHORN_V2_OPERATION": "inventory-replicas", "GITHUB_REF": "refs/heads/candidate", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_WORKFLOW_REF": "other-workflow", "GITHUB_SHA": "abbreviated",
		} {
			old := env[key]
			env[key] = bad
			if authorizeTemporaryPreparation(getenv, selection) == nil {
				t.Fatal("foreign or unconfirmed temporary preparation accepted:", key)
			}
			env[key] = old
		}
		for _, bad := range []string{"temporary-v2-1", "temporary-v1-3", "prod-control-plane-1", "prod-worker-1", "autoscale-longhorn-v1-0123456789abcdef", "10.0.99.4"} {
			env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = bad, "prepare-unused-longhorn-v2-"+bad
			if authorizeTemporaryPreparation(getenv, bad) == nil {
				t.Fatal("a nonempty, fresh-V2, permanent or arbitrary target was admitted")
			}
		}
	}
}

func TestRetainingPreparationUsesOnlyItsDistinctProtectedSelection(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "prepare-retained-temporary-worker"
	env["LONGHORN_V2_NODE"] = "temporary-v2-1"
	env["LONGHORN_V2_CONFIRM"] = "prepare-retained-longhorn-v2-temporary-v2-1"
	getenv := func(key string) string { return env[key] }
	if authorizeRetainedTemporaryPreparation(getenv, "temporary-v2-1") != nil ||
		authorizeTemporaryPreparation(getenv, "temporary-v2-1") == nil ||
		authorizeRetainedTemporaryPreparation(getenv, "temporary-v1-1") == nil {
		t.Fatal("retaining preparation lost its distinct protected scope")
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizeRetainedTemporaryPreparation(getenv, "temporary-v2-1") == nil {
		t.Fatal("a rerun could request retaining preparation")
	}
}
