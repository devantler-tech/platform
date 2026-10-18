package main

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCompletionWorkflowIsProtectedAndLeastPrivilege(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/complete-backstage-retirement.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if yaml.Unmarshal(b, &workflow) != nil {
		t.Fatal("completion workflow is invalid")
	}
	events := object(workflow["on"])
	job := object(field(workflow, "jobs", "complete"))
	permissions := object(job["permissions"])
	if len(events) != 1 || events["workflow_dispatch"] == nil || len(object(workflow["permissions"])) != 0 || !reflect.DeepEqual(permissions, map[string]any{"actions": "read", "contents": "read", "deployments": "read"}) || field(job, "environment", "name") != "prod" || field(workflow, "concurrency", "group") != "prod-deploy" || field(workflow, "concurrency", "cancel-in-progress") != false || field(workflow, "concurrency", "queue") != "max" {
		t.Fatal("completion lost its protected dispatch, environment, permission or serialization boundary")
	}
	inputs := object(field(events, "workflow_dispatch", "inputs"))
	if len(inputs) != 3 {
		t.Fatal("completion accepts operator-selected commands or targets")
	}
	for _, name := range []string{"confirmation", "deployed_revision", "accepted_deployment_run"} {
		if field(inputs, name, "type") != "string" || field(inputs, name, "required") != true {
			t.Fatal("completion input is not required and typed")
		}
	}
	steps := array(job["steps"])
	if len(steps) < 8 || field(object(steps[0]), "shell") != "bash" {
		t.Fatal("completion has no opening pre-credential admission")
	}
	guard := text(field(object(steps[0]), "run"))
	good := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_WORKFLOW_REF": completionWorkflow, "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "999", "GITHUB_SHA": strings.Repeat("a", 40),
		"PORTAL_RETIREMENT_CONFIRM": "COMPLETE BACKSTAGE RETIREMENT JOURNAL ONLY", "PORTAL_RETIREMENT_REVISION": "latest@sha256:" + strings.Repeat("b", 64), "PORTAL_RETIREMENT_DEPLOYMENT_RUN": "456",
	}
	for name, wrong := range map[string]string{"positive": "", "GITHUB_ACTIONS": "false", "GITHUB_REPOSITORY": "other/repo", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/branch", "GITHUB_WORKFLOW_REF": "other", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_RUN_ID": "", "GITHUB_SHA": "bad", "PORTAL_RETIREMENT_CONFIRM": "RETIRE BACKSTAGE STATELESS ONLY", "PORTAL_RETIREMENT_REVISION": "latest", "PORTAL_RETIREMENT_DEPLOYMENT_RUN": "none"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", guard)
			for key, value := range good {
				if key == name {
					value = wrong
				}
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			if err := cmd.Run(); (err == nil) != (name == "positive") {
				t.Fatalf("opening admission accepted wrong dispatch: error=%t", err != nil)
			}
		})
	}
	sourceStep, testsStep, credentialStep, executeStep := -1, -1, -1, -1
	for index, rawStep := range steps {
		step := object(rawStep)
		run := text(step["run"])
		if strings.Contains(run, "verify-prod-recovery-source.sh") {
			sourceStep = index
		}
		if strings.Contains(run, "go test -race -count=1 ./scripts/retire-backstage-stateless") {
			testsStep = index
		}
		if field(step, "env", "KUBE_CONFIG") != nil {
			credentialStep = index
		}
		if strings.Contains(run, "go run ./scripts/retire-backstage-stateless --complete") {
			executeStep = index
		}
		if strings.HasPrefix(text(step["uses"]), "actions/checkout@") && (field(step, "with", "ref") != "${{ github.sha }}" || field(step, "with", "persist-credentials") != false) {
			t.Fatal("checkout is not pinned to the dispatch commit")
		}
	}
	if sourceStep <= 0 || testsStep <= sourceStep || credentialStep <= testsStep || executeStep <= credentialStep {
		t.Fatal("credentials or terminal mutation precede current-main and regression proof")
	}
}

func TestDeploymentDefersOnlyTheApplicationRevisionCheckDuringTheHandoff(t *testing.T) {
	b, err := os.ReadFile("../../.github/actions/deploy-prod/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var action map[string]any
	if yaml.Unmarshal(b, &action) != nil {
		t.Fatal("deployment action is invalid")
	}
	matched := 0
	for _, rawStep := range array(field(action, "runs", "steps")) {
		step := object(rawStep)
		condition := text(step["if"])
		if strings.Contains(condition, "backstage_retirement_handoff") {
			matched++
			if step["name"] != "Verify the deployed data product UI release" || !strings.Contains(condition, "steps.reconcile.outputs.backstage_retirement_handoff != 'true'") {
				t.Fatalf("retirement handoff deferred an unrelated deployment check: %v", step["name"])
			}
		}
	}
	if matched != 1 {
		t.Fatalf("retirement handoff must defer exactly one revision-bound application check, found %d", matched)
	}
}
