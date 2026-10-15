package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Exercise the actual pre-credential guard and feed the committed final step's
// arguments to the real entrypoint. A workflow accidentally selecting execution
// instead of the check must fail rather than inherit a fixture's expectations.
func TestCheckWorkflowRequiresProtectedReadOnlyDispatch(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/check-backstage-retirement.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatal(err)
	}
	events := object(workflow["on"])
	job := object(field(workflow, "jobs", "check"))
	permissions := object(job["permissions"])
	if len(events) != 1 || events["workflow_dispatch"] == nil || len(object(workflow["permissions"])) != 0 || len(permissions) != 1 || permissions["contents"] != "read" || job["environment"] != "prod" || field(workflow, "concurrency", "group") != "prod-deploy" || field(workflow, "concurrency", "cancel-in-progress") != false {
		t.Fatal("read-only workflow lost protected dispatch/permission/concurrency boundaries")
	}
	inputs := object(field(events, "workflow_dispatch", "inputs"))
	if len(inputs) != 2 {
		t.Fatal("check accepts operator-selected commands or targets")
	}
	for _, name := range []string{"confirmation", "deployed_revision"} {
		if field(inputs, name, "type") != "string" || field(inputs, name, "required") != true {
			t.Fatal("check input is not required and typed")
		}
	}
	steps := array(job["steps"])
	if len(steps) < 2 || field(object(steps[0]), "shell") != "bash" {
		t.Fatal("no opening shell admission")
	}
	guard := text(field(object(steps[0]), "run"))
	base := map[string]string{
		"GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_EVENT_NAME": "workflow_dispatch",
		"GITHUB_WORKFLOW_REF":       "devantler-tech/platform/.github/workflows/check-backstage-retirement.yaml@refs/heads/main",
		"PORTAL_RETIREMENT_CONFIRM": "CHECK BACKSTAGE RETIREMENT ONLY", "PORTAL_RETIREMENT_REVISION": "latest@sha256:" + strings.Repeat("b", 64),
	}
	for key, invalid := range map[string]string{"valid": "", "GITHUB_REF": "refs/heads/branch", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_EVENT_NAME": "push", "GITHUB_WORKFLOW_REF": "other", "PORTAL_RETIREMENT_CONFIRM": "RETIRE BACKSTAGE STATELESS ONLY", "PORTAL_RETIREMENT_REVISION": "latest"} {
		t.Run(key, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", guard)
			for name, value := range base {
				if name == key {
					value = invalid
				}
				cmd.Env = append(cmd.Env, name+"="+value)
			}
			if err := cmd.Run(); (err == nil) != (key == "valid") {
				t.Fatalf("opening admission accepted wrong dispatch: error=%t", err != nil)
			}
		})
	}
	command := strings.Fields(text(field(object(steps[len(steps)-1]), "run")))
	if len(command) < 4 || command[0] != "go" || command[1] != "run" || command[2] != "./scripts/retire-backstage-stateless" {
		t.Fatal("check does not invoke the reviewed entrypoint")
	}
	checkRunner(t)
	f := retirementFixture(t)
	var out bytes.Buffer
	if code := run(command[3:], &out, f.execute); code != 0 || !strings.Contains(out.String(), "retirement=NOT_EXECUTED") || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted {
		t.Fatalf("workflow selected a write-capable or incomplete command: code=%d output=%q", code, out.String())
	}
}
