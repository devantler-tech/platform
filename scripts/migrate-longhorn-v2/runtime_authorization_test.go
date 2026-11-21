package main

import (
	"strings"
	"testing"
)

func TestRuntimeInventoryRequiresItsOwnConfirmedFirstMainAttempt(t *testing.T) {
	valid := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"GITHUB_SHA":          strings.Repeat("a", 40), "GITHUB_RUN_ID": "1234",
		"LONGHORN_V2_OPERATION": "inventory-v1-runtime", "LONGHORN_V2_CONFIRM": "inventory-v1-runtime",
	}
	if err := authorizeRuntimeInventory(func(key string) string { return valid[key] }); err != nil {
		t.Fatal("confirmed read-only operation refused:", err)
	}
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "GITHUB_WORKFLOW_REF", "GITHUB_SHA", "GITHUB_RUN_ID", "LONGHORN_V2_OPERATION", "LONGHORN_V2_CONFIRM", "LONGHORN_V2_NODE", "LONGHORN_V2_DATABASE"} {
		t.Run(key, func(t *testing.T) {
			getenv := func(name string) string {
				if name == key {
					return "foreign"
				}
				return valid[name]
			}
			if authorizeRuntimeInventory(getenv) == nil {
				t.Fatal("unconfirmed, targeted or foreign workflow reached runtime inventory")
			}
		})
	}
}
