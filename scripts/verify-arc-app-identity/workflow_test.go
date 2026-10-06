package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowProtectsCredentialsAndDoesNotActivateARC(t *testing.T) {
	workflow, err := readYAMLFile(filepath.Join("..", "..", ".github", "workflows", "verify-arc-app-identity.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	triggers := object(workflow["on"])
	if len(triggers) != 2 || triggers["workflow_dispatch"] == nil || triggers["pull_request"] == nil {
		t.Fatal("only manual production proof and ordinary PR fixtures are allowed")
	}
	if len(object(workflow["permissions"])) != 0 {
		t.Fatal("workflow defaults must grant no token permissions")
	}
	jobs := object(workflow["jobs"])
	job := object(jobs["identity"])
	concurrency := object(job["concurrency"])
	if concurrency["group"] != "prod-deploy" || concurrency["cancel-in-progress"] != false {
		t.Fatal("verification must serialize with protected deployment")
	}
	if len(jobs) != 2 || job["if"] != "${{ github.event_name == 'workflow_dispatch' }}" || job["environment"] != "prod" || job["runs-on"] != "ubuntu-latest" || job["timeout-minutes"] != 10 {
		t.Fatal("verification must use the protected bounded hosted-runner job")
	}
	permissions := object(job["permissions"])
	if len(permissions) != 1 || permissions["contents"] != "read" {
		t.Fatal("identity verification must have no GitHub write token")
	}
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatal("steps missing")
	}
	preflight, credentials, verifyIndex := -1, -1, -1
	guard := stringValue(object(steps[0])["run"])
	for _, required := range []string{"refs/heads/main", "verify-arc-app-identity", "GITHUB_RUN_ATTEMPT", "== 1"} {
		if !strings.Contains(guard, required) {
			t.Fatalf("protected invocation omits %s", required)
		}
	}
	for index, entry := range steps {
		step := object(entry)
		uses := stringValue(step["uses"])
		source := stringValue(step["run"])
		if strings.HasPrefix(uses, "actions/checkout@") {
			with := object(step["with"])
			if with["ref"] != "${{ github.sha }}" || with["persist-credentials"] != false {
				t.Fatal("source must bind immutable dispatch head without credential persistence")
			}
		}
		if strings.HasPrefix(uses, "actions/setup-go@") {
			if object(step["with"])["cache"] != false {
				t.Fatal("protected verifier must not restore an untrusted tool cache")
			}
		}
		if strings.Contains(source, "--preflight") {
			preflight = index
		}
		if strings.Contains(source, "--verify") {
			verifyIndex = index
		}
		for _, value := range object(step["env"]) {
			if strings.Contains(stringValue(value), "secrets.") {
				if preflight < 0 || index <= preflight {
					t.Fatal("credentials exposed before reviewed transport preflight")
				}
				if credentials < 0 {
					credentials = index
				}
			}
		}
		if strings.Contains(source, "${{ inputs.") || strings.Contains(uses, "upload-artifact") || strings.Contains(source, "bao kv") || strings.Contains(source, "kubectl apply") || strings.Contains(source, "helm ") {
			t.Fatal("identity workflow must not interpolate inputs, publish credential material or activate resources")
		}
	}
	if preflight < 0 || credentials <= preflight || verifyIndex <= credentials {
		t.Fatal("missing ordered transport/credential/identity stages")
	}
	fixtures := object(jobs["fixtures"])
	if fixtures["if"] != "${{ github.event_name == 'pull_request' }}" || fixtures["environment"] != nil || strings.Contains(stringValue(object(fixtures["concurrency"])["group"]), "prod-deploy") {
		t.Fatal("PR fixtures must have no protected environment or production lock")
	}
	fixtureSteps, _ := fixtures["steps"].([]any)
	tested := false
	for _, entry := range fixtureSteps {
		step := object(entry)
		source := stringValue(step["run"])
		if strings.Contains(source, "--verify") || strings.Contains(source, "kubectl") {
			t.Fatal("PR fixture job may not reach production")
		}
		for _, value := range object(step["env"]) {
			if strings.Contains(stringValue(value), "secrets.") {
				t.Fatal("PR fixture job has credentials")
			}
		}
		tested = tested || strings.Contains(source, "go test -race ./scripts/verify-arc-app-identity")
	}
	if !tested {
		t.Fatal("PR fixture behavior is not executed")
	}
}
