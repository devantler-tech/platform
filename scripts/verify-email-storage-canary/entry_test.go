package main

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutableRefusesUnconfirmedDispatchBeforeInvokingKubernetes(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "canary")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s", out)
	}
	fake := filepath.Join(dir, "kubectl")
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf called >> \"$CALL_LOG\"\nprintf private-tool-error >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "CALL_LOG=" + marker, "GITHUB_REPOSITORY=devantler-tech/platform", "GITHUB_REF=refs/heads/main", "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_RUN_ATTEMPT=1", "GITHUB_RUN_ID=123", "GITHUB_SHA=" + strings.Repeat("a", 40), "EMAIL_CANARY_CONFIRM=wrong", "RUNNER_ENVIRONMENT=github-hosted"}
	cmd := exec.Command(binary)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil || string(out) != "EMAIL_STORAGE_CANARY=UNVERIFIED stage=dispatch cleanup=NOT_STARTED\n" {
		t.Fatalf("unsafe dispatch not refused privately: %q", out)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Kubernetes was invoked before authorization")
	}
	for i, v := range env {
		if strings.HasPrefix(v, "EMAIL_CANARY_CONFIRM=") {
			env[i] = "EMAIL_CANARY_CONFIRM=verify-email-storage-canary"
		}
	}
	cmd = exec.Command(binary)
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	if err == nil || string(out) != "EMAIL_STORAGE_CANARY=UNVERIFIED stage=versions cleanup=VERIFIED run=123\n" {
		t.Fatalf("tool error escaped the closed receipt: %q", out)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "called" {
		t.Fatal("valid authorization did not reach the bounded preflight")
	}
}
func TestWorkflowProtectsDispatchAndPrivateInputs(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "verify-email-storage-canary.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow object
	if yaml.Unmarshal(source, &workflow) != nil {
		t.Fatal("invalid workflow")
	}
	on := child(workflow, "on")
	if len(on) != 1 || len(child(on, "workflow_dispatch")) == 0 {
		t.Fatal("canary gained an automatic trigger")
	}
	concurrency := child(workflow, "concurrency")
	if concurrency["group"] != "prod-deploy" || concurrency["cancel-in-progress"] != false {
		t.Fatal("canary no longer holds the production mutation lock")
	}
	job := child(child(workflow, "jobs"), "canary")
	if job["environment"] != "prod" || job["runs-on"] != "ubuntu-latest" || job["timeout-minutes"] != 40 {
		t.Fatal("protected hosted execution changed")
	}
	steps := items(job, "steps")
	if len(steps) != 6 {
		t.Fatal("review unexpected workflow steps")
	}
	checkout := child(steps[1], "with")
	if checkout["ref"] != "${{ github.sha }}" || checkout["persist-credentials"] != false {
		t.Fatal("checkout lost immutable revision or retained write credentials")
	}
	for _, guard := range []string{"GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "CONFIRM"} {
		if !strings.Contains(text(steps[0], "run"), guard) {
			t.Fatal("missing pre-credential dispatch guard:", guard)
		}
	}
	runtime := text(steps[5], "run")
	for _, required := range []string{"/dev/shm", "umask 077", "unset KUBE_CONFIG", "unset HCLOUD_TOKEN", "use-prod-stable-api-endpoint.sh", "trap 'rm -rf"} {
		if !strings.Contains(runtime, required) {
			t.Fatal("private-input lifecycle changed:", required)
		}
	}
	if strings.Contains(string(source), "upload-artifact") || strings.Contains(string(source), "port-forward") || strings.Contains(string(source), "OPENBAO") {
		t.Fatal("synthetic canary gained a recovery/credential export path")
	}
}
