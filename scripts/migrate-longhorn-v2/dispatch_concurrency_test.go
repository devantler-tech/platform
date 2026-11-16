package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestManualMigrationValidationSharesTheProductionTurnWithoutNestedLocks(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	for _, validate := range []func(object) error{validateMigrationDispatchConcurrency, validateMigrationDispatchDependencies, validateMigrationDispatchBoundarySteps, validateMigrationDispatchSourceSteps} {
		if err := validate(workflow); err != nil {
			t.Fatal(err)
		}
	}
}

func migrationDispatchWorkflow(t *testing.T) object {
	t.Helper()
	raw, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.YAMLToJSONStrict(raw)
	var workflow object
	if err != nil || json.Unmarshal(data, &workflow) != nil {
		t.Fatal("workflow is not strict YAML")
	}
	return workflow
}

func validateMigrationDispatchConcurrency(workflow object) error {
	group := "${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && github.run_attempt == 1 && 'prod-deploy' || format('longhorn-v2-validation-{0}-{1}', github.run_id, github.run_attempt) }}"
	if text(workflow, "concurrency", "group") != group || value(workflow, "concurrency", "cancel-in-progress") != false || text(workflow, "concurrency", "queue") != "max" {
		return errors.New("manual validation can be overtaken, cancelled, or share the production lock with PR validation")
	}
	jobs := valueObject(workflow, "jobs")
	for name, raw := range jobs {
		job, ok := raw.(object)
		if !ok || value(job, "concurrency") != nil {
			return errors.New("nested job lock can block its own workflow: " + name)
		}
	}
	return nil
}

func validateMigrationDispatchDependencies(workflow object) error {
	if len(valueObject(workflow, "permissions")) != 0 || text(workflow, "jobs", "tests", "permissions", "contents") != "read" ||
		text(workflow, "jobs", "prepare", "needs") != "tests" || text(workflow, "jobs", "prepare", "if") != "${{ github.event_name == 'workflow_dispatch' }}" ||
		text(workflow, "jobs", "prepare", "environment") != "prod" {
		return errors.New("moving the lock changed the protected operation dependency or permission boundary")
	}
	return nil
}

func validateMigrationDispatchBoundarySteps(workflow object) error {
	tests, _ := value(workflow, "jobs", "tests", "steps").([]any)
	tested := false
	for _, raw := range tests {
		step := raw.(object)
		if text(step, "run") == "go test -race -count=1 -timeout=5m ./scripts/migrate-longhorn-v2 ./internal/longhornmigration ./internal/longhorninventory" {
			tested = true
		}
	}
	if !tested {
		return errors.New("the complete boundary tests must still finish before the operation")
	}
	return nil
}

func validateMigrationDispatchSourceSteps(workflow object) error {
	steps, _ := value(workflow, "jobs", "prepare", "steps").([]any)
	positions := map[string]int{}
	for i, raw := range steps {
		step := raw.(object)
		positions[text(step, "name")] = i + 1
		if text(step, "name") == "Require current main" && strings.TrimSpace(text(step, "run")) != `./scripts/verify-prod-recovery-source.sh "${GITHUB_SHA}"` {
			return errors.New("serialization must not replace source freshness")
		}
		if text(step, "name") == "Checkout reviewed source" && (text(step, "with", "ref") != "${{ github.sha }}" || value(step, "with", "persist-credentials") != false) {
			return errors.New("the dispatch must keep its original reviewed source")
		}
	}
	credentials := positions["Restore production credentials in memory"]
	for _, gate := range []string{"Require confirmed first attempt on main", "Checkout reviewed source", "Require current main"} {
		if positions[gate] == 0 || credentials == 0 || positions[gate] >= credentials {
			return errors.New("credentials preceded the original authorization or freshness gate: " + gate)
		}
	}
	return nil
}

func TestMigrationDispatchRejectsSkippedOrFailureTolerantSafetyChecks(t *testing.T) {
	base, err := json.Marshal(migrationDispatchWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ job, step string }{
		{"tests", ""}, {"prepare", ""},
		{"tests", "Test complete observations and interrupted recovery"},
		{"prepare", "Require confirmed first attempt on main"},
		{"prepare", "Checkout reviewed source"},
		{"prepare", "Require current main"},
	} {
		for _, override := range []struct {
			field string
			value any
		}{
			{"if", false}, {"if", "${{ false }}"},
			{"continue-on-error", true}, {"continue-on-error", "${{ true }}"},
		} {
			t.Run(scope.job+"/"+scope.step+"/"+override.field+"/"+stringifyDispatchOverride(override.value), func(t *testing.T) {
				var workflow object
				if json.Unmarshal(base, &workflow) != nil {
					t.Fatal("fixture decode")
				}
				target := valueObject(workflow, "jobs", scope.job)
				if scope.step != "" {
					target = migrationDispatchStep(t, target, scope.step)
				}
				target[override.field] = override.value
				if validateMigrationDispatchDependencies(workflow) == nil && validateMigrationDispatchBoundarySteps(workflow) == nil && validateMigrationDispatchSourceSteps(workflow) == nil {
					t.Fatal("disabled or failure-tolerant safety check was accepted")
				}
			})
		}
	}
}

func stringifyDispatchOverride(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func migrationDispatchStep(t *testing.T, job object, name string) object {
	t.Helper()
	steps, _ := job["steps"].([]any)
	for _, raw := range steps {
		step := raw.(object)
		if text(step, "name") == name {
			return step
		}
	}
	t.Fatal("required fixture step missing", name)
	return nil
}
