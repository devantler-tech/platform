package emailcredentials_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type verificationStep struct {
	Name, Uses, Run, Shell string
	Env                    map[string]string
	With                   map[string]any
}

// verificationSteps validates the dispatch-only workflow and its execution boundary.
func verificationSteps(t *testing.T) []verificationStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), ".github/workflows/verify-email-credential-custody.yaml"))
	if err != nil {
		t.Fatal("missing guarded Email custody verification workflow", err)
	}
	var workflow struct {
		On          map[string]any
		Permissions map[string]string
		Jobs        map[string]struct {
			RunsOn      string `yaml:"runs-on"`
			Environment string
			Permissions map[string]string
			Steps       []verificationStep
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job, ok := workflow.Jobs["verify"]
	if len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || !ok || len(workflow.Jobs) != 1 {
		t.Fatal("custody verification must be dispatch-only with no ambient write permissions")
	}
	if job.RunsOn != "ubuntu-latest" || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) != 4 {
		t.Fatal("verification must retain its hosted, protected, read-only execution boundary")
	}
	checkout := job.Steps[1]
	if checkout.Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" || checkout.With["ref"] != "${{ github.sha }}" || checkout.With["persist-credentials"] != false {
		t.Fatal("verification must check out the confirmed immutable revision without persisted credentials")
	}
	return job.Steps
}

// TestVerificationGuardRunsBeforeCheckoutOrSecrets executes permitted and refused dispatches.
func TestVerificationGuardRunsBeforeCheckoutOrSecrets(t *testing.T) {
	steps := verificationSteps(t)
	guard := steps[0]
	if guard.Uses != "" || guard.Shell != "bash" || len(guard.Env) != 1 || guard.Env["CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("the first step must guard dispatch before any checkout or secret access")
	}
	for _, test := range []struct {
		name, event, ref, repo, confirm string
		allowed                         bool
	}{
		{"approved", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "verify-email-credential-custody", true},
		{"pull request", "pull_request", "refs/heads/main", "devantler-tech/platform", "verify-email-credential-custody", false},
		{"branch", "workflow_dispatch", "refs/heads/codex/other", "devantler-tech/platform", "verify-email-credential-custody", false},
		{"repository", "workflow_dispatch", "refs/heads/main", "other/platform", "verify-email-credential-custody", false},
		{"confirmation", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "copy", false},
		{"shell input", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "$(exit 0)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("bash", "-eu", "-c", guard.Run)
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=" + test.event, "GITHUB_REF=" + test.ref, "GITHUB_REPOSITORY=" + test.repo, "CONFIRM=" + test.confirm}
			if err := command.Run(); (err == nil) != test.allowed {
				t.Fatal("dispatch guard admitted an unconfirmed execution or refused main")
			}
		})
	}
}

// TestVerificationWrapperRefusesIncompleteProofAndNeverCopies rejects unexamined or partial results.
func TestVerificationWrapperRefusesIncompleteProofAndNeverCopies(t *testing.T) {
	step := verificationSteps(t)[3]
	if len(step.Env) != 3 || step.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || step.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || step.Env["EMAIL_CUSTODY_MODE"] != "plan" {
		t.Fatal("verification must use only existing credentials and force read-only planning")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the unchanged workflow wrapper requires Linux /dev/shm; Linux CI executes all refusal cases")
	}
	for _, test := range []struct {
		name, proof string
		allowed     bool
	}{
		{"pending", "EMAIL_CUSTODY_PREPARATION=PLANNED mode=plan existing=0 copied=0 pending=3 unseeded=0", true},
		{"matching", "EMAIL_CUSTODY_PREPARATION=PLANNED mode=plan existing=3 copied=0 pending=0 unseeded=0", true},
		{"mixed", "EMAIL_CUSTODY_PREPARATION=PLANNED mode=plan existing=1 copied=0 pending=2 unseeded=0", true},
		{"missing", "EMAIL_CUSTODY_PREPARATION=UNSEEDED mode=plan existing=0 copied=0 pending=0 unseeded=3", false},
		{"incomplete", "EMAIL_CUSTODY_PREPARATION=PLANNED mode=plan existing=1 copied=0 pending=1 unseeded=0", false},
		{"copy", "EMAIL_CUSTODY_PREPARATION=VERIFIED mode=copy existing=0 copied=3 pending=0 unseeded=0", false},
		{"private output", "unexpected private response", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, path := range []string{"bin", "scripts"} {
				if err := os.Mkdir(filepath.Join(dir, path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				"bin/findmnt": "#!/bin/bash\nprintf 'tmpfs\\n'\n",
				"bin/curl":    "#!/bin/bash\nexit 0\n",
				"bin/kubectl": "#!/bin/bash\nset -eu\ncase \"$*\" in\n'-n openbao get secret openbao-unseal -o jsonpath={.data.root-token}') printf 'dGVzdC1vbmx5LXJvb3QtdG9rZW4=';;\n'-n openbao port-forward --address=127.0.0.1 service/openbao-active 38200:8200') exec sleep 60;;\n*) exit 91;;\nesac\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/bash\nexit 0\n",
				"scripts/prepare-email-credentials.sh":    "#!/bin/bash\nset -eu\n[[ $EMAIL_CUSTODY_MODE == plan && $EMAIL_CUSTODY_WORK_DIR == /dev/shm/email-custody.* && -s $EMAIL_CUSTODY_TOKEN_FILE ]] || exit 92\nprintf '%s\\n' \"$SYNTHETIC_PROOF\"\n",
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// The wrapper itself runs unchanged. Only its external reader/API and
			// helper boundary are synthetic; any unexpected kubectl action fails.
			command := exec.Command("bash", "-eu", "-c", step.Run)
			command.Dir = dir
			command.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + dir, "KUBE_CONFIG=synthetic-kubeconfig", "HCLOUD_TOKEN=synthetic-provider-token", "EMAIL_CUSTODY_MODE=copy", "SYNTHETIC_PROOF=" + test.proof}
			out, err := command.CombinedOutput()
			if (err == nil) != test.allowed {
				t.Fatalf("verification wrapper accepted incomplete proof or refused all-three plan: %v %s", err, out)
			}
			if strings.Contains(string(out), "synthetic-") || strings.Contains(string(out), "unexpected private response") {
				t.Fatal("verification wrapper exposed private input or unvalidated output")
			}
		})
	}
}
