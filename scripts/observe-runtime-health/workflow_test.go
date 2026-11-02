package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReviewedWorkflowBoundary(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/observe-runtime-health.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Jobs        map[string]struct {
			Environment string            `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Env         map[string]string `yaml:"env"`
			Steps       []struct {
				If              string            `yaml:"if"`
				ContinueOnError any               `yaml:"continue-on-error"`
				Run             string            `yaml:"run"`
				Uses            string            `yaml:"uses"`
				Env             map[string]string `yaml:"env"`
				With            map[string]any    `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Env) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("observer is not dispatch-only with empty top-level grants")
	}
	job := workflow.Jobs["observe"]
	if job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" ||
		len(job.Env) != 1 || job.Env["RUNTIME_HEALTH_CONFIRM"] != "${{ inputs.confirm }}" || len(job.Steps) < 4 {
		t.Fatal("observer permissions, confirmation or environment changed")
	}
	if job.Steps[0].Run == "" || job.Steps[0].Uses != "" || job.Steps[0].If != "" ||
		(job.Steps[0].ContinueOnError != nil && job.Steps[0].ContinueOnError != false) || strings.Count(string(data), "secrets.") != 2 {
		t.Fatal("dispatch refusal must precede checkout")
	}
	checkout := job.Steps[1]
	if checkout.Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" ||
		checkout.With["ref"] != "${{ github.sha }}" || checkout.With["persist-credentials"] != false {
		t.Fatal("checkout is not bound to the reviewed dispatch revision")
	}
	buildSeen := false
	for i, step := range job.Steps {
		if strings.HasPrefix(step.Run, "go build -o ") {
			buildSeen = true
		}
		if strings.Contains(step.Uses, "upload-") {
			t.Fatal("private responses must not be uploaded")
		}
		if strings.Contains(step.Env["KUBE_CONFIG"], "secrets.") {
			if i != len(job.Steps)-1 || !buildSeen || len(step.Env) != 2 ||
				step.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || step.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" {
				t.Fatal("credentials are not confined to the final read step after compilation")
			}
			for _, fragment := range []string{"set +x", "umask 077", "mktemp -d /dev/shm/", "trap '", "unset KUBE_CONFIG", "unset HCLOUD_TOKEN", `"$RUNNER_TEMP/observe-runtime-health"`} {
				if !strings.Contains(step.Run, fragment) {
					t.Fatalf("missing credential boundary: %s", fragment)
				}
			}
		}
	}
	guard := job.Steps[0].Run
	good := map[string]string{
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch",
		"GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "RUNTIME_HEALTH_CONFIRM": "observe-runtime-health",
	}
	run := func() error {
		cmd := exec.Command("bash", "-c", guard)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for k, v := range good {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return cmd.Run()
	}
	if err := run(); err != nil {
		t.Fatalf("valid dispatch guard failed: %v", err)
	}
	for key, bad := range map[string]string{"GITHUB_REPOSITORY": "foreign/platform", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/proposed", "GITHUB_RUN_ATTEMPT": "2", "RUNTIME_HEALTH_CONFIRM": "yes"} {
		old := good[key]
		good[key] = bad
		if run() == nil {
			t.Fatalf("actual workflow guard accepted unsafe %s", key)
		}
		good[key] = old
	}
}
