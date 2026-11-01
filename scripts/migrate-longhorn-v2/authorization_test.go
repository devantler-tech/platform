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
		if authorizeRefresh(getenv) == nil {
			t.Fatal("benchmark dispatch authorized another operation")
		}
		env["GITHUB_REF"] = "refs/heads/unreviewed"
		if authorizeBenchmark(getenv, phase) == nil {
			t.Fatal("unreviewed branch benchmark authorized")
		}
	}
}
