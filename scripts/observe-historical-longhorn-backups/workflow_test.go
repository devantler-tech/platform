package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowRetainsProductionReadBoundaries(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/observe-historical-longhorn-backups.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			Environment string            `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Needs       string            `yaml:"needs"`
			Concurrency struct {
				Group  string `yaml:"group"`
				Cancel bool   `yaml:"cancel-in-progress"`
				Queue  string `yaml:"queue"`
			} `yaml:"concurrency"`
			Steps []struct {
				Name string            `yaml:"name"`
				If   string            `yaml:"if"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Permissions == nil || len(workflow.Permissions) != 0 || len(workflow.On) != 3 || workflow.On["workflow_dispatch"] == nil || workflow.On["pull_request"] == nil {
		t.Fatal("workflow event or permission boundary changed")
	}
	if _, ok := workflow.On["merge_group"]; !ok {
		t.Fatal("missing merge-group validation")
	}
	job := workflow.Jobs["observe"]
	if job.If != "${{ github.event_name == 'workflow_dispatch' }}" || job.Environment != "prod" || job.Needs != "tests" ||
		len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || job.Concurrency.Group != "prod-deploy" || job.Concurrency.Cancel || job.Concurrency.Queue != "max" {
		t.Fatal("protected read job boundary changed")
	}
	if len(job.Steps) < 2 || !strings.Contains(job.Steps[0].Run, `"${GITHUB_RUN_ATTEMPT}" == 1`) || !strings.Contains(job.Steps[0].Run, `refs/heads/main`) ||
		!strings.Contains(job.Steps[0].Run, `observe-historical-longhorn-backups`) {
		t.Fatal("first attempt/ref/confirmation gate missing")
	}
	checkout, source, restore, execute, cleanup := -1, -1, -1, -1, -1
	for i, step := range job.Steps {
		if step.With["ref"] == "${{ github.sha }}" && step.With["persist-credentials"] == "false" {
			checkout = i
		}
		if step.Run == `./scripts/verify-prod-recovery-source.sh "${GITHUB_SHA}"` && step.Env["GH_TOKEN"] == "${{ github.token }}" {
			source = i
		}
		if step.Env["KUBE_CONFIG"] == "${{ secrets.KUBE_CONFIG }}" && strings.Contains(step.Run, "/dev/shm/longhorn-catalogue.") {
			restore = i
		}
		if step.Run == "go run ./scripts/observe-historical-longhorn-backups" {
			execute = i
		}
		if step.If == "${{ always() }}" && strings.Contains(step.Run, "LONGHORN_CATALOGUE_CREDENTIAL_DIR") {
			cleanup = i
		}
	}
	if checkout < 1 || source <= checkout || restore <= source || execute <= restore || cleanup <= execute {
		t.Fatal("source/credential/execution/cleanup ordering changed")
	}
}
