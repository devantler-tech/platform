package main

import (
	"bytes"
	"testing"
)

func groupInvocationFixture(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "31415", "ARC_RUNNER_GROUP_CONFIRM": "verify-arc-runner-group-capability", "GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/verify-arc-runner-group-capability.yaml@refs/heads/main", "GITHUB_SHA": "788e960618a0ca1eeb3f7eb45061b24cc9287e45", "GITHUB_WORKFLOW_SHA": "788e960618a0ca1eeb3f7eb45061b24cc9287e45"} {
		t.Setenv(name, value)
	}
}

func TestRunnerGroupCommandProtectedInvocation(t *testing.T) {
	root := configFixture(t)
	groupInvocationFixture(t)
	var output bytes.Buffer
	calls := 0
	code := runRunnerGroup([]string{"--verify-runner-group"}, root, &output, func(configuration) outcome { calls++; return "PASS_EXISTING" })
	if code != 0 || calls != 1 || output.String() != "ARC_RUNNER_GROUP_CAPABILITY=PASS_EXISTING\n" {
		t.Fatalf("got code %d, calls %d, output %q", code, calls, output.String())
	}
}

func TestRunnerGroupCommandRejectsEveryWrongBinding(t *testing.T) {
	for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "GITHUB_RUN_ID", "ARC_RUNNER_GROUP_CONFIRM", "GITHUB_WORKFLOW_REF", "GITHUB_SHA", "GITHUB_WORKFLOW_SHA"} {
		t.Run(name, func(t *testing.T) {
			root := configFixture(t)
			groupInvocationFixture(t)
			t.Setenv(name, "sensitive-rejected-value")
			var output bytes.Buffer
			code := runRunnerGroup([]string{"--verify-runner-group"}, root, &output, func(configuration) outcome { t.Fatal("unprotected invocation reached runtime"); return pass })
			if code != 1 || output.String() != "ARC_RUNNER_GROUP_CAPABILITY=HOLD_INVOCATION\n" {
				t.Fatalf("got code %d, output %q", code, output.String())
			}
		})
	}
}

func TestRunnerGroupCommandHasOnlyFixedVerdicts(t *testing.T) {
	for _, result := range []outcome{"PASS_DISPOSABLE", "HOLD_CAPABILITY", "HOLD_OWNERSHIP", failCleanup, "secret-response-canary"} {
		root := configFixture(t)
		groupInvocationFixture(t)
		var output bytes.Buffer
		code := runRunnerGroup([]string{"--verify-runner-group"}, root, &output, func(configuration) outcome { return result })
		want := result
		if result == "secret-response-canary" {
			want = failAPI
		}
		if output.String() != "ARC_RUNNER_GROUP_CAPABILITY="+string(want)+"\n" {
			t.Fatal("unbounded output")
		}
		if (code == 0) != (result == "PASS_DISPOSABLE") {
			t.Fatal("wrong exit status")
		}
	}
}

func TestRunnerGroupCommandPreflightNeverUsesCredentials(t *testing.T) {
	root := configFixture(t)
	var output bytes.Buffer
	code := runRunnerGroup([]string{"--preflight-runner-group"}, root, &output, func(configuration) outcome { t.Fatal("preflight reached production"); return pass })
	if code != 0 || output.String() != "ARC_RUNNER_GROUP_CAPABILITY=TRANSPORT_CONFIG_READY\n" {
		t.Fatal("wrong preflight result")
	}
}
