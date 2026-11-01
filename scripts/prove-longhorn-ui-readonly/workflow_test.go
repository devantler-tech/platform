package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHostedProofKeepsCredentialsBehindReviewedManualCode(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prove-longhorn-ui-readonly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Group  string `yaml:"group"`
			Cancel bool   `yaml:"cancel-in-progress"`
			Queue  string `yaml:"queue"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			Environment string            `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run  string            `yaml:"run"`
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("proof must remain manual with no root token grant or extra jobs")
	}
	if workflow.Concurrency.Group != "prod-deploy" || workflow.Concurrency.Cancel || workflow.Concurrency.Queue != "max" {
		t.Fatal("proof must serialize with production deployments")
	}
	job := workflow.Jobs["prove"]
	if job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) != 6 {
		t.Fatal("unexpected credentialed proof surface")
	}
	for _, guard := range []string{"${GITHUB_REPOSITORY}\" == devantler-tech/platform", "${GITHUB_EVENT_NAME}\" == workflow_dispatch", "${GITHUB_REF}\" == refs/heads/main", "${GITHUB_RUN_ATTEMPT}\" == 1", "${LONGHORN_UI_READ_PROOF_CONFIRM}\" == prove-longhorn-ui-readonly"} {
		if !strings.Contains(job.Steps[0].Run, guard) {
			t.Fatalf("missing guard before checkout: %s", guard)
		}
	}
	if !strings.HasPrefix(job.Steps[1].Uses, "actions/checkout@") || job.Steps[1].With["ref"] != "${{ github.sha }}" || job.Steps[1].With["persist-credentials"] != "false" {
		t.Fatal("checkout must bind the immutable main dispatch revision")
	}
	for _, requirement := range []string{"mktemp -d /dev/shm/longhorn-ui-proof.XXXXXX", "trap 'rm -rf \"$proof_dir\"' EXIT", "unset KUBE_CONFIG", "unset HCLOUD_TOKEN", "\"$RUNNER_TEMP/prove-longhorn-ui-readonly\""} {
		if !strings.Contains(job.Steps[5].Run, requirement) {
			t.Fatalf("missing private fixed-probe requirement: %s", requirement)
		}
	}
}
