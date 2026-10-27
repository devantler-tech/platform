package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionRecoveryRefusesBranchBeforeCheckoutOrCredentials(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "verify-openbao-backup-recovery.yaml"))
	if err != nil {
		t.Fatal("independent backup recovery workflow is missing")
	}
	type step struct {
		Name, Uses, Shell, Run string
		Env                    map[string]string
		With                   map[string]any
	}
	var workflow struct {
		On          map[string]any `yaml:"on"`
		Permissions map[string]string
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			Environment string
			Permissions map[string]string
			Steps       []step
		}
	}
	if yaml.Unmarshal(data, &workflow) != nil || len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 || workflow.Concurrency.Group != "prod-deploy" || workflow.Concurrency.Cancel {
		t.Fatal("workflow execution boundary changed")
	}
	job, ok := workflow.Jobs["verify"]
	if !ok || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) != 5 {
		t.Fatal("verification must use one protected read-only job")
	}
	guard := job.Steps[0]
	if guard.Uses != "" || guard.Shell != "bash" || len(guard.Env) != 1 || guard.Env["CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("first step must refuse before checkout and secret injection")
	}
	for _, tc := range []struct {
		event, ref, repo, confirm string
		pass                      bool
	}{
		{"workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "verify-openbao-backup-recovery", true},
		{"pull_request", "refs/heads/main", "devantler-tech/platform", "verify-openbao-backup-recovery", false},
		{"workflow_dispatch", "refs/heads/codex/other", "devantler-tech/platform", "verify-openbao-backup-recovery", false},
		{"workflow_dispatch", "refs/heads/main", "other/platform", "verify-openbao-backup-recovery", false},
		{"workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "$(exit 0)", false},
	} {
		command := exec.Command("bash", "-eu", "-c", guard.Run)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=" + tc.event, "GITHUB_REF=" + tc.ref, "GITHUB_REPOSITORY=" + tc.repo, "CONFIRM=" + tc.confirm}
		if err := command.Run(); (err == nil) != tc.pass {
			t.Fatal("guard admitted an unconfirmed dispatch or refused main")
		}
	}
	checkout := job.Steps[1]
	if checkout.Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" || checkout.With["ref"] != "${{ github.sha }}" || checkout.With["persist-credentials"] != false {
		t.Fatal("checkout is not pinned to the confirmed revision")
	}
	for _, s := range job.Steps[:4] {
		if len(s.Env) > 1 || strings.Contains(s.Run, "secrets.") {
			t.Fatal("credential access moved before the guarded reader")
		}
	}
	run := job.Steps[4]
	if len(run.Env) != 2 || run.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || run.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || strings.Contains(run.Run, "upload-artifact") {
		t.Fatal("recovery reader credential boundary changed")
	}
	for _, required := range []string{"/dev/shm", "OPENBAO_RECOVERY_DIR", "service/openbao-active 38200:8200", "run-openbao-backup-sandbox.sh verify", "OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3"} {
		if !strings.Contains(run.Run, required) {
			t.Fatal("missing private isolated recovery boundary")
		}
	}
}
