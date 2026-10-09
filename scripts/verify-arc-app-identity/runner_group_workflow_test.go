package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerGroupWorkflowProtectsProofAndCredentialCleanup(t *testing.T) {
	workflow, err := readYAMLFile(filepath.Join("..", "..", ".github", "workflows", "verify-arc-runner-group-capability.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	triggers := object(workflow["on"])
	if len(triggers) != 1 || triggers["workflow_dispatch"] == nil || len(object(workflow["permissions"])) != 0 {
		t.Fatal("unsafe event or permission defaults")
	}
	jobs := object(workflow["jobs"])
	job := object(jobs["capability"])
	if len(jobs) != 1 || job["environment"] != "prod" || job["runs-on"] != "ubuntu-latest" || job["timeout-minutes"] != 10 || job["if"] != "${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' }}" {
		t.Fatal("production admission changed")
	}
	lock := object(job["concurrency"])
	if lock["group"] != "prod-deploy" || lock["cancel-in-progress"] != false || lock["queue"] != "max" {
		t.Fatal("production lock missing")
	}
	permissions := object(job["permissions"])
	if len(permissions) != 1 || permissions["contents"] != "read" {
		t.Fatal("proof must not use a GitHub write job token")
	}
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatal("steps missing")
	}
	preflight, restore, proof, cleanup := -1, -1, -1, -1
	for index, item := range steps {
		step := object(item)
		uses := stringValue(step["uses"])
		run := stringValue(step["run"])
		if strings.HasPrefix(uses, "actions/checkout@") && (object(step["with"])["persist-credentials"] != false || object(step["with"])["ref"] != "${{ github.sha }}") {
			t.Fatal("unbound source checkout")
		}
		if strings.HasPrefix(uses, "actions/setup-go@") && object(step["with"])["cache"] != false {
			t.Fatal("protected tool cache must remain disabled")
		}
		if strings.Contains(run, "--preflight-runner-group") {
			preflight = index
		}
		if strings.Contains(run, "--verify-runner-group") {
			proof = index
		}
		if step["id"] == "kubeconfig" {
			restore = index
			if !strings.Contains(run, "noclobber") || !strings.Contains(run, "owned=true") || !strings.Contains(run, "umask 077") {
				t.Fatal("credential file ownership not recorded")
			}
		}
		if strings.Contains(run, "rm -f") {
			cleanup = index
			if step["if"] != "${{ always() && steps.kubeconfig.outputs.owned == 'true' }}" || !strings.Contains(run, "${KUBECONFIG}") {
				t.Fatal("cleanup must cover partial restoration and only the owned file")
			}
		}
		for _, value := range object(step["env"]) {
			if strings.Contains(stringValue(value), "secrets.") && (preflight < 0 || index <= preflight) {
				t.Fatal("production credentials precede preflight")
			}
		}
		if strings.Contains(uses, "create-github-app-token") || strings.Contains(run, "gh auth") || strings.Contains(run, "kubectl apply") || strings.Contains(run, "helm install") {
			t.Fatal("proof broadened its activation or token boundary")
		}
	}
	if preflight < 0 || restore <= preflight || proof <= restore || cleanup <= proof {
		t.Fatal("proof ordering incomplete")
	}
	identity, err := readYAMLFile(filepath.Join("..", "..", ".github", "workflows", "verify-arc-app-identity.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := object(object(identity["jobs"])["fixtures"])
	if fixture["if"] != "${{ github.event_name == 'pull_request' }}" || fixture["environment"] != nil {
		t.Fatal("PR fixtures reached protected access")
	}
	paths, ok := object(object(identity["on"])["pull_request"])["paths"].([]any)
	if !ok {
		t.Fatal("fixture paths missing")
	}
	watched := false
	for _, path := range paths {
		if path == ".github/workflows/verify-arc-runner-group-capability.yaml" {
			watched = true
		}
	}
	if !watched {
		t.Fatal("workflow-only changes bypass credential-free fixtures")
	}
}
