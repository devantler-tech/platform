package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowHasPrivateBoundedCleanupPath(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/verify-email-provider-smtp.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow object
	if yaml.Unmarshal(data, &workflow) != nil {
		t.Fatal("invalid workflow YAML")
	}
	if len(at(workflow, "on").(map[string]any)) != 1 || at(workflow, "on", "workflow_dispatch") == nil || at(workflow, "concurrency", "group") != "prod-deploy" || at(workflow, "concurrency", "cancel-in-progress") != false {
		t.Fatal("probe is not confirmed, main-only serialized work")
	}
	job := at(workflow, "jobs", "probe").(map[string]any)
	if at(job, "environment") != "prod" || at(job, "timeout-minutes") != 15 || len(at(job, "permissions").(map[string]any)) != 1 || at(job, "permissions", "contents") != "read" {
		t.Fatal("workflow access boundary changed")
	}
	built, checkedOut, proof, cleanup := false, false, false, false
	for _, step := range rows(job, "steps") {
		if strings.HasPrefix(str(step, "uses"), "actions/checkout@") {
			checkedOut = at(step, "with", "ref") == "${{ github.sha }}" && at(step, "with", "persist-credentials") == false
		}
		if strings.Contains(str(step, "run"), "go build -o") {
			built = checkedOut
		}
		if at(step, "env", "KUBE_CONFIG") != nil {
			if !built || at(step, "env", "KUBE_CONFIG") != "${{ secrets.KUBE_CONFIG }}" || at(step, "env", "HCLOUD_TOKEN") != "${{ secrets.HCLOUD_TOKEN }}" {
				t.Fatal("credentials preceded reviewed build or binding changed")
			}
			switch str(step, "run") {
			case "bash scripts/verify-email-provider/run.sh":
				proof = true
			case "bash scripts/verify-email-provider/run.sh --cleanup":
				cleanup = str(step, "if") == "${{ always() && steps.probe.outcome != 'skipped' }}"
			default:
				t.Fatal("unexpected credential-bearing command")
			}
		}
	}
	if !proof || !cleanup {
		t.Fatal("proof or interruption cleanup is missing")
	}
}

func TestCredentialWrapperClearsEnvironmentAndFiles(t *testing.T) {
	for _, failAPI := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "api-refusal"}[failAPI], func(t *testing.T) {
			dir := t.TempDir()
			private := filepath.Join(dir, "ram-fixture")
			if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(private, 0700); err != nil {
				t.Fatal(err)
			}
			fixtures := map[string]string{
				"findmnt": "#!/bin/sh\nprintf 'tmpfs\\n'\n",
				"mktemp":  "#!/bin/sh\nprintf '%s\\n' \"$PROBE_PRIVATE_FIXTURE\"\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/sh\ntest \"$HCLOUD_TOKEN\" = PRIVATE_TOKEN_FIXTURE\nprintf 'PRIVATE_PROVIDER_ERROR' >&2\nexit \"$PROBE_API_EXIT\"\n",
				"verify-email-provider":                   "#!/bin/sh\nset -eu\ntest -z \"${HCLOUD_TOKEN+x}\"\ntest -z \"${KUBE_CONFIG+x}\"\ntest \"$(cat \"$KUBECONFIG\")\" = PRIVATE_KUBECONFIG_FIXTURE\nprintf 'FIXTURE_DRIVER_INVOKED\\n'\n",
			}
			for name, contents := range fixtures {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := filepath.Abs("run.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/bash", script)
			cmd.Dir = dir
			apiExit := "0"
			if failAPI {
				apiExit = "1"
			}
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "PROBE_PRIVATE_FIXTURE=" + private, "PROBE_API_EXIT=" + apiExit, "GITHUB_REPOSITORY=devantler-tech/platform", "GITHUB_ACTOR=devantler", "GITHUB_REF=refs/heads/main", "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_RUN_ATTEMPT=1", "RUNNER_ENVIRONMENT=github-hosted", "EMAIL_PROVIDER_CONFIRM=verify-email-provider-smtp", "RUNNER_TEMP=" + dir, "KUBE_CONFIG=PRIVATE_KUBECONFIG_FIXTURE", "HCLOUD_TOKEN=PRIVATE_TOKEN_FIXTURE"}
			output, runErr := cmd.CombinedOutput()
			want := "FIXTURE_DRIVER_INVOKED\n"
			if failAPI {
				want = "EMAIL_PROVIDER_SMTP=UNVERIFIED stage=stable-api\n"
			}
			if (runErr != nil) != failAPI || string(output) != want {
				t.Fatalf("wrapper: %v %q", runErr, output)
			}
			if _, err := os.Stat(private); !os.IsNotExist(err) {
				t.Fatal("private kubeconfig survived wrapper cleanup")
			}
		})
	}
}
