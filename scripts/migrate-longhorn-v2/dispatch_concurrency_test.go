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
	if !migrationDispatchMustRun(valueObject(workflow, "jobs", "tests")) || !migrationDispatchMustSucceed(valueObject(workflow, "jobs", "prepare")) {
		return errors.New("migration safety jobs can be skipped or tolerate failure")
	}
	if len(valueObject(workflow, "permissions")) != 0 || text(workflow, "jobs", "tests", "permissions", "contents") != "read" ||
		text(workflow, "jobs", "prepare", "needs") != "tests" || text(workflow, "jobs", "prepare", "if") != "${{ github.event_name == 'workflow_dispatch' }}" ||
		text(workflow, "jobs", "prepare", "environment") != "prod" {
		return errors.New("moving the lock changed the protected operation dependency or permission boundary")
	}
	return nil
}

func validateMigrationDispatchBoundarySteps(workflow object) error {
	tests, _ := value(workflow, "jobs", "tests", "steps").([]any)
	required := map[string]bool{
		"bash scripts/tests/test-longhorn-runtime-inventory-source.sh":                                                                                             false,
		"go test -race -count=1 -timeout=7m ./scripts/migrate-longhorn-v2 ./internal/longhornmigration ./internal/longhorninventory ./internal/namespaceinventory": false,
	}
	for _, raw := range tests {
		step := raw.(object)
		command := text(step, "run")
		if _, safetyTest := required[command]; safetyTest {
			if !migrationDispatchMustRun(step) {
				return errors.New("boundary tests can be skipped or tolerate failure")
			}
			required[command] = true
		}
	}
	for command, tested := range required {
		if !tested {
			return errors.New("required pre-operation test is missing: " + command)
		}
	}
	return nil
}

func TestMigrationDispatchRequiresEveryBoundaryPackage(t *testing.T) {
	for _, pkg := range []string{"./scripts/migrate-longhorn-v2", "./internal/longhornmigration", "./internal/longhorninventory", "./internal/namespaceinventory"} {
		workflow := migrationDispatchWorkflow(t)
		step := migrationDispatchStep(t, valueObject(workflow, "jobs", "tests"), "Test complete observations and interrupted recovery")
		step["run"] = strings.ReplaceAll(text(step, "run"), " "+pkg, "")
		if validateMigrationDispatchBoundarySteps(workflow) == nil {
			t.Fatal("an omitted boundary package was admitted:", pkg)
		}
	}
}

const migrationDispatchFreshness = `set -euo pipefail
if [[ "${LONGHORN_V2_OPERATION}" == inventory-v1-runtime ]]; then
  source_sha="$(bash .github/scripts/select-longhorn-runtime-inventory-source.sh)"
  ./scripts/verify-prod-recovery-source.sh "${source_sha}"
  printf 'sha=%s\n' "${source_sha}" >>"${GITHUB_OUTPUT}"
  printf 'LONGHORN_RUNTIME_INVENTORY_SOURCE=BOUND sha=%s\n' "${source_sha}"
else
  ./scripts/verify-prod-recovery-source.sh "${GITHUB_SHA}"
fi`

func validateMigrationDispatchSourceSteps(workflow object) error {
	steps, _ := value(workflow, "jobs", "prepare", "steps").([]any)
	positions := map[string]int{}
	for i, raw := range steps {
		step := raw.(object)
		positions[text(step, "name")] = i + 1
		switch text(step, "name") {
		case "Require confirmed first attempt on main", "Checkout reviewed source", "Require current main":
			if !migrationDispatchMustRun(step) {
				return errors.New("authorization or source gate can be skipped or tolerate failure")
			}
		}
		switch text(step, "name") {
		case "Require current main":
			if strings.TrimSpace(text(step, "run")) != migrationDispatchFreshness || text(step, "id") != "recovery-source" || text(step, "env", "GH_TOKEN") != "${{ github.token }}" {
				return errors.New("only the read-only inventory may rebind tested source before the main freshness gate")
			}
		case "Checkout reviewed source":
			if text(step, "with", "ref") != "${{ github.sha }}" || value(step, "with", "persist-credentials") != false {
				return errors.New("the dispatch must check out its original reviewed source")
			}
		case "Run the confirmed migration preparation":
			if text(step, "env", "LONGHORN_RUNTIME_INVENTORY_SOURCE") != "${{ steps.recovery-source.outputs.sha }}" {
				return errors.New("the inventory must read the source selected by the freshness gate")
			}
		}
	}
	credentials := positions["Restore production credentials in memory"]
	for _, gate := range []string{"Require confirmed first attempt on main", "Checkout reviewed source", "Require current main"} {
		if positions[gate] == 0 || credentials == 0 || positions[gate] >= credentials {
			return errors.New("credentials preceded the original authorization or freshness gate: " + gate)
		}
	}
	if positions["Run the confirmed migration preparation"] <= credentials {
		return errors.New("the confirmed operation must follow credential setup")
	}
	return nil
}

func migrationDispatchMustRun(scope object) bool {
	return value(scope, "if") == nil && migrationDispatchMustSucceed(scope)
}

func migrationDispatchMustSucceed(scope object) bool {
	policy := value(scope, "continue-on-error")
	return policy == nil || policy == false
}

func TestMigrationDispatchAllowsExplicitFailureIntolerance(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	for _, job := range []string{"tests", "prepare"} {
		scope := valueObject(workflow, "jobs", job)
		scope["continue-on-error"] = false
		steps, _ := scope["steps"].([]any)
		for _, raw := range steps {
			raw.(object)["continue-on-error"] = false
		}
	}
	for _, validate := range []func(object) error{validateMigrationDispatchDependencies, validateMigrationDispatchBoundarySteps, validateMigrationDispatchSourceSteps} {
		if err := validate(workflow); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationDispatchRejectsSkippedOrFailureTolerantSafetyChecks(t *testing.T) {
	base, err := json.Marshal(migrationDispatchWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ job, step string }{
		{"tests", ""}, {"prepare", ""},
		{"tests", "Test queued read-only source selection"},
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

func TestMigrationDispatchRejectsAnUnboundRuntimeInventory(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	prepare := valueObject(workflow, "jobs", "prepare")
	gate := migrationDispatchStep(t, prepare, "Require current main")
	gate["run"] = strings.Replace(migrationDispatchFreshness, "inventory-v1-runtime", "retain-provider-source", 1)
	if validateMigrationDispatchSourceSteps(workflow) == nil {
		t.Fatal("a storage mutation was allowed to rebind its dispatch source")
	}
	gate["run"] = migrationDispatchFreshness
	run := migrationDispatchStep(t, prepare, "Run the confirmed migration preparation")
	valueObject(run, "env")["LONGHORN_RUNTIME_INVENTORY_SOURCE"] = "${{ github.sha }}"
	if validateMigrationDispatchSourceSteps(workflow) == nil {
		t.Fatal("the reader was allowed to ignore the verified source")
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
