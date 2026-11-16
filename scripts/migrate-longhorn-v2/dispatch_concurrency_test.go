package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestManualMigrationValidationSharesTheProductionTurnWithoutNestedLocks(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.YAMLToJSONStrict(raw)
	var workflow object
	if err != nil || json.Unmarshal(data, &workflow) != nil {
		t.Fatal("workflow is not strict YAML")
	}
	group := "${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && github.run_attempt == 1 && 'prod-deploy' || format('longhorn-v2-validation-{0}-{1}', github.run_id, github.run_attempt) }}"
	if text(workflow, "concurrency", "group") != group || value(workflow, "concurrency", "cancel-in-progress") != false || text(workflow, "concurrency", "queue") != "max" {
		t.Fatal("manual validation can be overtaken, cancelled, or share the production lock with PR validation")
	}
	jobs := valueObject(workflow, "jobs")
	for name, raw := range jobs {
		job, ok := raw.(object)
		if !ok || value(job, "concurrency") != nil {
			t.Fatal("nested job lock can block its own workflow", name)
		}
	}
	if len(valueObject(workflow, "permissions")) != 0 || text(workflow, "jobs", "tests", "permissions", "contents") != "read" ||
		text(workflow, "jobs", "prepare", "needs") != "tests" || text(workflow, "jobs", "prepare", "if") != "${{ github.event_name == 'workflow_dispatch' }}" ||
		text(workflow, "jobs", "prepare", "environment") != "prod" {
		t.Fatal("moving the lock changed the protected operation dependency or permission boundary")
	}
	tests, _ := value(workflow, "jobs", "tests", "steps").([]any)
	tested := false
	for _, raw := range tests {
		step := raw.(object)
		if text(step, "run") == "go test -race -count=1 -timeout=5m ./scripts/migrate-longhorn-v2 ./internal/longhornmigration ./internal/longhorninventory" {
			tested = true
		}
	}
	if !tested {
		t.Fatal("the complete boundary tests must still finish before the operation")
	}
	steps, _ := value(workflow, "jobs", "prepare", "steps").([]any)
	positions := map[string]int{}
	for i, raw := range steps {
		step := raw.(object)
		positions[text(step, "name")] = i + 1
		if text(step, "name") == "Require current main" && strings.TrimSpace(text(step, "run")) != `./scripts/verify-prod-recovery-source.sh "${GITHUB_SHA}"` {
			t.Fatal("serialization must not replace source freshness")
		}
		if text(step, "name") == "Checkout reviewed source" && (text(step, "with", "ref") != "${{ github.sha }}" || value(step, "with", "persist-credentials") != false) {
			t.Fatal("the dispatch must keep its original reviewed source")
		}
	}
	credentials := positions["Restore production credentials in memory"]
	for _, gate := range []string{"Require confirmed first attempt on main", "Checkout reviewed source", "Require current main"} {
		if positions[gate] == 0 || credentials == 0 || positions[gate] >= credentials {
			t.Fatal("credentials preceded the original authorization or freshness gate", gate)
		}
	}
}
