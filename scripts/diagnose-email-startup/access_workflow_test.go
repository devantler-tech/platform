package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAccessWorkflowRequiresASeparateMainOnlyOptIn checks the shipped entrypoint.
func TestAccessWorkflowRequiresASeparateMainOnlyOptIn(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/read-email-probe-state.yaml")
	if err != nil {
		t.Fatal("missing bounded probe-state workflow:", err)
	}
	var w observationWorkflow
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	if len(w.On) != 1 || w.On["workflow_dispatch"] == nil || w.Concurrency.Group != "prod-deploy" || w.Concurrency.Cancel || len(w.Permissions) != 0 || len(w.Jobs) != 1 {
		t.Fatal("unsafe dispatch/serialization contract")
	}
	j := w.Jobs["observe"]
	if j.Environment != "prod" || j.Timeout != 10 || len(j.Permissions) != 1 || j.Permissions["contents"] != "read" || j.Env["MAIL_ACCESS_CONFIRM"] != "$"+"{{ inputs.confirm }}" || len(j.Steps) != 6 {
		t.Fatal("unsafe job contract")
	}
	guard := j.Steps[0].Run
	for _, needle := range []string{"GITHUB_REPOSITORY", "devantler-tech/platform", "GITHUB_EVENT_NAME", "workflow_dispatch", "GITHUB_REF", "refs/heads/main", "GITHUB_RUN_ATTEMPT", "MAIL_ACCESS_CONFIRM", "read-email-probe-state"} {
		if !strings.Contains(guard, needle) {
			t.Fatalf("missing dispatch guard %s", needle)
		}
	}
	if j.Steps[1].With["ref"] != "$"+"{{ github.sha }}" || j.Steps[1].With["persist-credentials"] != false || !strings.Contains(j.Steps[3].Run, "./scripts/diagnose-email-startup") {
		t.Fatal("source/build binding lost")
	}
	for i, s := range j.Steps {
		if i < 5 {
			for _, v := range s.Env {
				if strings.Contains(v, "secrets.") {
					t.Fatal("credentials before build")
				}
			}
		}
		if strings.Contains(s.Run, "upload-artifact") {
			t.Fatal("private evidence must not be uploaded")
		}
	}
	if j.Steps[1].Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" || j.Steps[2].With["cache"] != false {
		t.Fatal("checkout/toolchain pin changed")
	}
	baseline := readObservationWorkflow(t).Jobs["observe"].Steps[5]
	if j.Steps[5].Run != strings.ReplaceAll(baseline.Run, "MAIL_STARTUP_SAMPLE", "MAIL_PROBE_STATE") || len(j.Steps[5].Env) != 2 || j.Steps[5].Env["KUBE_CONFIG"] != baseline.Env["KUBE_CONFIG"] || j.Steps[5].Env["HCLOUD_TOKEN"] != baseline.Env["HCLOUD_TOKEN"] {
		t.Fatal("credential shell differs from tested isolation/cleanup contract")
	}
	for _, key := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_ACCESS_CONFIRM", "valid"} {
		env := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "MAIL_ACCESS_CONFIRM": "read-email-probe-state"}
		if key != "valid" {
			env[key] = "unapproved"
		}
		cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", guard)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, e := cmd.CombinedOutput()
		if len(out) != 0 || (e == nil) != (key == "valid") {
			t.Fatalf("dispatch guard %s: %v", key, e)
		}
	}
}
