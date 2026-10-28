package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWorkflowDispatchGuardRunsBeforeProtectedSteps(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/read-crossplane-alert-recovery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("observer is not restricted to the opt-in dispatch")
	}
	job, ok := workflow.Jobs["observe"]
	if !ok || len(job.Steps) == 0 || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("observer job permissions or guard unavailable")
	}
	guard := job.Steps[0].Run
	if guard == "" || len(job.Steps[0].Env) != 0 {
		t.Fatal("first step must be the credential-free dispatch guard")
	}
	valid := dispatchEnvironment()
	for _, changed := range []string{"", "GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "CROSSPLANE_RECOVERY_CONFIRM"} {
		t.Run(changed, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-c", guard)
			command.Env = []string{"PATH=/usr/bin:/bin"}
			for key, value := range valid {
				if key == changed {
					value = "invalid"
				}
				command.Env = append(command.Env, key+"="+value)
			}
			err := command.Run()
			if (changed == "") != (err == nil) {
				t.Fatalf("actual workflow guard admitted the wrong dispatch: changed=%s err=%v", changed, err)
			}
		})
	}
}
