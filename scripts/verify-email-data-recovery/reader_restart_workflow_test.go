package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestReaderRestartWorkflowKeepsOneConfirmedMainOnlyPreparation exercises its actual dispatch guard.
func TestReaderRestartWorkflowKeepsOneConfirmedMainOnlyPreparation(t *testing.T) {
	t.Chdir("../..")
	raw, err := os.ReadFile(".github/workflows/prepare-retained-email-reader-witness.yaml")
	if err != nil {
		t.Fatalf("the complete restart-witness operator is not wired: %v", err)
	}
	var workflow struct {
		On          map[string]any `json:"on"`
		Permissions map[string]string
		Concurrency struct {
			Group, Queue string
			Cancel       bool `json:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			Environment string
			Timeout     int `json:"timeout-minutes"`
			Permissions map[string]string
			Steps       []struct {
				Run  string
				Env  map[string]string
				With map[string]any
			}
		}
	}
	if yaml.Unmarshal(raw, &workflow) != nil || len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || workflow.Concurrency.Group != "prod-deploy" || workflow.Concurrency.Queue != "max" || workflow.Concurrency.Cancel || len(workflow.Jobs) != 1 {
		t.Fatal("restart workflow widened triggers, permissions or production serialization")
	}
	job := workflow.Jobs["prepare"]
	if len(job.Steps) != 6 || job.Environment != "prod" || job.Timeout != 25 || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("restart preparation lacks its bounded protected job")
	}
	for _, changed := range []string{"", "GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF", "GITHUB_ACTOR", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "CONFIRM"} {
		env := readerRestartDispatch()
		env["CONFIRM"] = "prepare-retained-email-reader-witness"
		if changed != "" {
			env[changed] = "foreign"
		}
		command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", job.Steps[0].Run+"\nprintf ADMITTED")
		command.Env = nil
		for key, value := range env {
			command.Env = append(command.Env, key+"="+value)
		}
		output, err := command.CombinedOutput()
		if (err == nil) != (changed == "") || strings.Contains(string(output), "ADMITTED") != (changed == "") {
			t.Fatal("restart workflow admitted a foreign dispatch or refused the confirmed caller")
		}
	}
	if job.Steps[1].With["ref"] != "${{ github.sha }}" || job.Steps[1].With["persist-credentials"] != false {
		t.Fatal("restart workflow checkout is not immutable and credential free")
	}
	for _, step := range job.Steps[:5] {
		for _, value := range step.Env {
			if strings.Contains(value, "secrets.") {
				t.Fatal("credentials were restored before setup and builds completed")
			}
		}
	}
	if job.Steps[5].Run != "bash scripts/prepare-retained-email-reader-witness.sh" || len(child(workflow.On, "workflow_dispatch")) == 0 {
		t.Fatal("restart workflow has no fixed actual operator")
	}
	for _, name := range []string{"EMAIL_RETAINED_READER_UID", "EMAIL_RETAINED_CLAIM_UID", "EMAIL_RETAINED_PV_UID", "EMAIL_RETAINED_BACKEND_UID", "EMAIL_RETAINED_REPLICA_UIDS"} {
		if job.Steps[5].Env[name] != "${{ secrets."+name+" }}" {
			t.Fatal("restart workflow lost an independent native witness")
		}
	}
	ci, err := os.ReadFile(".github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"scripts/prepare-retained-email-reader-witness.sh", ".github/workflows/prepare-retained-email-reader-witness.yaml"} {
		if !strings.Contains(string(ci), "- '"+path+"'") {
			t.Fatal("restart preparation changes omitted the existing native recovery CI family")
		}
	}
}
