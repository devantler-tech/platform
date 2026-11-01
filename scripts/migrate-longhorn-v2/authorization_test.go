package main

import "testing"

func dispatchFixture() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ID": "1234", "GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_SHA": "1234567890123456789012345678901234567890", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"LONGHORN_V2_CONFIRM": "prepare-longhorn-v2-prod-worker-1", "LONGHORN_V2_NODE": "prod-worker-1",
		"LONGHORN_V2_OPERATION": "prepare-worker",
	}
}

func TestMutationRequiresTheReviewedNodePreparationDispatch(t *testing.T) {
	env := dispatchFixture()
	if err := authorize(func(k string) string { return env[k] }, "prod-worker-1"); err != nil {
		t.Fatal(err)
	}
	for key, wrong := range map[string]string{
		"GITHUB_ACTIONS": "false", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/candidate",
		"GITHUB_REPOSITORY": "other/platform", "GITHUB_RUN_ID": "", "GITHUB_RUN_ATTEMPT": "2",
		"GITHUB_SHA": "1234", "RUNNER_ENVIRONMENT": "self-hosted", "GITHUB_WORKFLOW_REF": "other-workflow",
		"LONGHORN_V2_CONFIRM": "", "LONGHORN_V2_NODE": "prod-worker-2",
		"LONGHORN_V2_OPERATION": "enable-capacity",
	} {
		t.Run(key, func(t *testing.T) {
			e := dispatchFixture()
			e[key] = wrong
			if err := authorize(func(k string) string { return e[k] }, "prod-worker-1"); err == nil {
				t.Fatal("foreign or unconfirmed dispatch may mutate production")
			}
		})
	}
	if err := authorize(func(string) string { return "" }, "prod-worker-1"); err == nil {
		t.Fatal("local execution may mutate production")
	}
	env["LONGHORN_V2_NODE"] = "prod-control-plane-1"
	if err := authorize(func(k string) string { return env[k] }, "prod-control-plane-1"); err == nil {
		t.Fatal("control plane reset was authorized")
	}
}

func TestCapacityActivationHasItsOwnExplicitOperationAndConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "enable-capacity"
	env["LONGHORN_V2_CONFIRM"] = "enable-longhorn-migration-capacity-v1"
	env["LONGHORN_V2_POOL"] = "v1"
	if err := authorizeCapacity(func(k string) string { return env[k] }, "v1"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LONGHORN_V2_OPERATION", "LONGHORN_V2_CONFIRM", "LONGHORN_V2_POOL", "GITHUB_REF", "GITHUB_WORKFLOW_REF", "GITHUB_RUN_ATTEMPT"} {
		t.Run(key, func(t *testing.T) {
			old := env[key]
			env[key] = "invalid"
			defer func() { env[key] = old }()
			if err := authorizeCapacity(func(k string) string { return env[k] }, "v1"); err == nil {
				t.Fatal("unconfirmed or foreign capacity activation accepted")
			}
		})
	}
	if err := authorizeCapacity(func(k string) string { return env[k] }, "other-pool"); err == nil {
		t.Fatal("arbitrary pool could be armed for storage")
	}
}

func TestClientRefreshRequiresItsOwnConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "refresh-clients"
	env["LONGHORN_V2_CONFIRM"] = "refresh-longhorn-v2-clients"
	if err := authorizeRefresh(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LONGHORN_V2_OPERATION", "LONGHORN_V2_CONFIRM", "GITHUB_REF", "GITHUB_RUN_ATTEMPT"} {
		old := env[key]
		env[key] = "invalid"
		if err := authorizeRefresh(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("client refresh accepted invalid %s", key)
		}
		env[key] = old
	}
}

func TestBenchmarkAuthorizationDoesNotAuthorizeOtherMigrationWrites(t *testing.T) {
	for _, phase := range []string{"v1", "v2"} {
		env := dispatchFixture()
		env["LONGHORN_V2_OPERATION"] = "benchmark-" + phase
		env["LONGHORN_V2_CONFIRM"] = "benchmark-longhorn-storage"
		getenv := func(key string) string { return env[key] }
		if err := authorizeBenchmark(getenv, phase); err != nil {
			t.Fatal(err)
		}
		if authorize(getenv, "prod-worker-1") == nil || authorizeCapacity(getenv, "v1") == nil || authorizeRefresh(getenv) == nil {
			t.Fatal("benchmark dispatch authorized another operation")
		}
		env["GITHUB_REF"] = "refs/heads/unreviewed"
		if authorizeBenchmark(getenv, phase) == nil {
			t.Fatal("unreviewed branch benchmark authorized")
		}
	}
}

func TestConfirmationMustNameTheSelectedTarget(t *testing.T) {
	worker := dispatchFixture()
	worker["LONGHORN_V2_NODE"] = "prod-worker-2"
	for _, confirmation := range []string{"prepare-longhorn-v2-prod-worker-1", "prepare-longhorn-v2-node"} {
		worker["LONGHORN_V2_CONFIRM"] = confirmation
		if err := authorize(func(k string) string { return worker[k] }, "prod-worker-2"); err == nil {
			t.Fatalf("%s confirmed a reset of prod-worker-2", confirmation)
		}
	}
	worker["LONGHORN_V2_CONFIRM"] = "prepare-longhorn-v2-prod-worker-2"
	if err := authorize(func(k string) string { return worker[k] }, "prod-worker-2"); err != nil {
		t.Fatal(err)
	}
	pool := dispatchFixture()
	pool["LONGHORN_V2_OPERATION"] = "enable-capacity"
	pool["LONGHORN_V2_POOL"] = "v2"
	for _, confirmation := range []string{"enable-longhorn-migration-capacity-v1", "enable-longhorn-migration-capacity"} {
		pool["LONGHORN_V2_CONFIRM"] = confirmation
		if err := authorizeCapacity(func(k string) string { return pool[k] }, "v2"); err == nil {
			t.Fatalf("%s confirmed the v2 pool", confirmation)
		}
	}
	pool["LONGHORN_V2_CONFIRM"] = "enable-longhorn-migration-capacity-v2"
	if err := authorizeCapacity(func(k string) string { return pool[k] }, "v2"); err != nil {
		t.Fatal(err)
	}
}
