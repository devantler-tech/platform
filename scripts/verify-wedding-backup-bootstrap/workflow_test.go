package main

import (
	"os"
	"strings"
	"testing"
)

func TestWorkflowRequiresProtectedManualInvocationAndIndependentCleanup(t *testing.T) {
	w, err := readObject("../../.github/workflows/verify-wedding-backup-bootstrap.yaml")
	if err != nil {
		t.Fatal("protected workflow missing")
	}
	on, ok := w["on"].(map[string]any)
	if !ok || len(on) != 1 || on["workflow_dispatch"] == nil {
		t.Fatal("proof can run automatically")
	}
	if str(w, "concurrency", "group") != "prod-deploy" || at(w, "concurrency", "cancel-in-progress") != false {
		t.Fatal("production deployment lock missing")
	}
	job := at(w, "jobs", "bootstrap").(map[string]any)
	if str(job, "environment") != "prod" {
		t.Fatal("unprotected environment")
	}
	steps := job["steps"].([]any)
	guard := steps[0].(map[string]any)
	for _, text := range []string{"refs/heads/main", "verify-wedding-backup-bootstrap", "GITHUB_RUN_ATTEMPT"} {
		if !strings.Contains(str(guard, "run"), text) {
			t.Fatal("invocation guard missing")
		}
	}
	clean := steps[len(steps)-1].(map[string]any)
	if !strings.Contains(str(clean, "if"), "always()") || !strings.Contains(str(clean, "if"), "steps.invocation.outcome == 'success'") || str(clean, "run") != "go run ./scripts/verify-wedding-backup-bootstrap --cleanup" {
		t.Fatal("independent cleanup missing")
	}
	ci, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil || !strings.Contains(string(ci), "go test ./scripts/verify-wedding-backup-bootstrap") {
		t.Fatal("regression suite absent from CI")
	}
}
