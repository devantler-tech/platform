package main

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The actual workflow guard must reject branch, actor and rerun changes before
// checkout or credentials; metadata checks preserve the fixed disposal handoff.
func TestRetainedDisposalWorkflowMainGuardAndCustody(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/dispose-retained-email-copy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			Environment string
			Timeout     int `yaml:"timeout-minutes"`
			Permissions map[string]string
			Steps       []struct {
				Uses, Run, Shell string
				With             map[string]any
				Env              map[string]string
			}
		}
	}
	if yaml.Unmarshal(data, &w) != nil || len(w.On) != 1 || w.On["workflow_dispatch"] == nil || len(w.Permissions) != 0 || len(w.Jobs) != 1 || w.Concurrency.Group != "prod-deploy" || w.Concurrency.Cancel {
		t.Fatal("workflow widened its trigger, permission or concurrency boundary")
	}
	job := w.Jobs["dispose"]
	if job.Environment != "prod" || job.Timeout > 25 || job.Timeout < 3 || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) != 6 {
		t.Fatal("observation custody changed")
	}
	guard := job.Steps[0]
	env := retainedDispatch()
	env["CONFIRM"] = "dispose-retained-email-copy"
	for _, key := range append([]string{""}, "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF", "GITHUB_ACTOR", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "CONFIRM") {
		t.Run("guard/"+key, func(t *testing.T) {
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", guard.Run+"\nprintf DISPOSAL_ADMITTED\n")
			for k, v := range env {
				if k == key {
					v = "foreign"
				}
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			out, err := cmd.CombinedOutput()
			if key == "" {
				if err != nil || string(out) != "DISPOSAL_ADMITTED" {
					t.Fatal("confirmed dispatch refused")
				}
			} else if err == nil || strings.Contains(string(out), "DISPOSAL_ADMITTED") {
				t.Fatal("foreign workflow admitted")
			}
		})
	}
	checkout := job.Steps[1]
	if checkout.With["ref"] != "${{ github.sha }}" || checkout.With["persist-credentials"] != false || !strings.HasPrefix(checkout.Uses, "actions/checkout@") {
		t.Fatal("checkout lost immutable credential-free binding")
	}
	for i, step := range job.Steps {
		if i < 5 && (step.Env["KUBE_CONFIG"] != "" || step.Env["HCLOUD_TOKEN"] != "") {
			t.Fatal("credential restored before build")
		}
	}
	final := job.Steps[5]
	if final.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || final.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || final.Env["EMAIL_RETAINED_COPY_CONFIRM"] != "${{ inputs.confirm }}" || !strings.HasSuffix(strings.TrimSpace(final.Run), `"$RUNNER_TEMP/verify-email-data-recovery" --dispose-retained`) {
		t.Fatal("workflow invoked recovery writes or lost credential binding")
	}
	if final.Env["EMAIL_RETAINED_READER_UID"] != "${{ secrets.EMAIL_RETAINED_READER_UID }}" || final.Env["EMAIL_RETAINED_CLAIM_UID"] != "${{ secrets.EMAIL_RETAINED_CLAIM_UID }}" || !strings.Contains(final.Run, `./scripts/verify-prod-recovery-source.sh "$GITHUB_SHA"`) {
		t.Fatal("disposal lost independent identities or current reviewed source guard")
	}
	for _, step := range job.Steps {
		if strings.Contains(step.Uses, "upload-artifact") {
			t.Fatal("private observation output became a public artifact")
		}
	}
}
