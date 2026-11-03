package main

import (
	"os"
	"os/exec"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTemporaryActivationRequiresItsOwnFirstAttemptMainConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "register-temporary-v2-storage"
	env["LONGHORN_V2_CONFIRM"] = "register-longhorn-v2-temporary-storage"
	getenv := func(key string) string { return env[key] }
	if err := authorizeTemporaryActivation(getenv); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"LONGHORN_V2_CONFIRM": "prepare-longhorn-v2-prod-worker-1", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_REF": "refs/heads/topic", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_WORKFLOW_REF": "other/workflow@main"} {
		t.Run(key, func(t *testing.T) {
			old := env[key]
			env[key] = value
			defer func() { env[key] = old }()
			if authorizeTemporaryActivation(getenv) == nil {
				t.Fatal("unreviewed or differently confirmed activation was accepted")
			}
		})
	}
	if authorizeCanary(getenv) == nil || authorizeHostReboot(getenv, "prod-worker-1") == nil {
		t.Fatal("disk registration authorized a canary or host disruption")
	}
}

func TestTemporaryActivationWorkflowRequiresTheDistinctConfirmation(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Run string } }
	}
	if yaml.Unmarshal(data, &workflow) != nil {
		t.Fatal("invalid workflow")
	}
	for _, confirmation := range []string{"register-longhorn-v2-temporary-storage", "verify-longhorn-v2-restore", "prepare-unused-longhorn-v2-temporary-v1-1"} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", workflow.Jobs["prepare"].Steps[0].Run)
		cmd.Env = append(os.Environ(), "DISPATCH_REF=refs/heads/main", "GITHUB_RUN_ATTEMPT=1", "LONGHORN_V2_OPERATION=register-temporary-v2-storage", "LONGHORN_V2_CONFIRM="+confirmation)
		if err = cmd.Run(); (err == nil) != (confirmation == "register-longhorn-v2-temporary-storage") {
			t.Fatalf("registration confirmation %s: %v", confirmation, err)
		}
	}
}
