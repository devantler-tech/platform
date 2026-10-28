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

func custodyCopySteps(t *testing.T) []verificationStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), ".github/workflows/copy-email-credential-custody.yaml"))
	if err != nil {
		t.Fatal("guarded custody-copy workflow is missing", err)
	}
	var workflow struct {
		On          map[string]any
		Permissions map[string]string
		Jobs        map[string]struct {
			Environment string
			Permissions map[string]string
			Steps       []verificationStep
		}
	}
	if yaml.Unmarshal(data, &workflow) != nil || len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("custody copy must have only its confirmed dispatch and no ambient permissions")
	}
	job, ok := workflow.Jobs["copy"]
	if !ok || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) < 3 {
		t.Fatal("custody copy lost its protected read-only GitHub boundary")
	}
	return job.Steps
}

// A missing first-attempt or immutable-ref guard would admit privileged execution.
func TestCustodyCopyGuardRefusesUnconfirmedExecution(t *testing.T) {
	guard := custodyCopySteps(t)[0]
	if guard.Uses != "" || guard.Env["CONFIRM"] != "${{ inputs.confirm }}" || len(guard.Env) != 1 {
		t.Fatal("confirmation must run before checkout or secret access")
	}
	for _, test := range []struct {
		name, event, ref, repo, attempt, confirm string
		allowed                                  bool
	}{
		{"confirmed", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "1", "copy-email-credentials-after-recovery", true},
		{"event", "pull_request", "refs/heads/main", "devantler-tech/platform", "1", "copy-email-credentials-after-recovery", false},
		{"branch", "workflow_dispatch", "refs/heads/codex/other", "devantler-tech/platform", "1", "copy-email-credentials-after-recovery", false},
		{"repository", "workflow_dispatch", "refs/heads/main", "other/platform", "1", "copy-email-credentials-after-recovery", false},
		{"retry", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "2", "copy-email-credentials-after-recovery", false},
		{"missing attempt", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "", "copy-email-credentials-after-recovery", false},
		{"confirmation", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "1", "copy", false},
		{"injection", "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "1", "$(exit 0)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("bash", "-eu", "-c", guard.Run)
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=" + test.event, "GITHUB_REF=" + test.ref, "GITHUB_REPOSITORY=" + test.repo, "GITHUB_RUN_ATTEMPT=" + test.attempt, "CONFIRM=" + test.confirm}
			if err := command.Run(); (err == nil) != test.allowed {
				t.Fatal("custody copy admitted an unconfirmed execution or refused the confirmed first attempt")
			}
		})
	}
}

// Skipping recovery, trusting malformed output or accepting incomplete totals
// must change the observable copy attempt, exit status or public receipt.
func TestCustodyCopyRequiresRecoveryBeforeAnyCopy(t *testing.T) {
	steps := custodyCopySteps(t)
	var step verificationStep
	for _, candidate := range steps {
		if candidate.Env["KUBE_CONFIG"] != "" {
			step = candidate
		}
	}
	if step.Run == "" || len(step.Env) != 2 || step.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || step.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" {
		t.Fatal("copy must use only its fixed existing deployment credentials")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the unchanged production wrapper requires Linux private tmpfs; hosted Go CI runs the behavioral cases")
	}
	for _, test := range []struct {
		name, mode      string
		copied, allowed bool
	}{
		{"three copied", "success", true, true},
		{"three existing", "existing", true, true},
		{"mixed", "mixed", true, true},
		{"failed collection", "collection-failure", false, false},
		{"malformed collection", "collection-format", false, false},
		{"failed restore", "restore-failure", false, false},
		{"malformed restore", "restore-format", false, false},
		{"failed copy", "copy-failure", true, false},
		{"incomplete copy", "copy-partial", true, false},
		{"private copy output", "copy-private", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, path := range []string{"bin", "scripts"} {
				if os.Mkdir(filepath.Join(dir, path), 0700) != nil {
					t.Fatal("could not prepare synthetic workspace")
				}
			}
			files := map[string]string{
				"bin/findmnt": "#!/bin/bash\nprintf 'tmpfs\\n'\n",
				"bin/kubectl": "#!/bin/bash\n[[ $* == '--context=admin@prod -n openbao port-forward --address=127.0.0.1 service/openbao-active 38200:8200' ]] || exit 91\nexec sleep 60\n",
				"bin/curl":    "#!/bin/bash\n[[ $* == *http://127.0.0.1:38200/v1/sys/health* ]] || exit 91\n",
				"bin/docker":  "#!/bin/bash\n[[ $* == 'pull synthetic-pinned-server' ]] || exit 91\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/bash\nexit 0\n",
				"verify-openbao-recovery": `#!/bin/bash
set -eu
case "$*" in
 collect)
   [[ $OPENBAO_RECOVERY_DIR == /dev/shm/email-custody-copy.* && -s $KUBECONFIG ]] || exit 91
   printf '%s' "$OPENBAO_RECOVERY_DIR" > "$SYNTHETIC_CASE_DIR/scratch"
   [[ $SYNTHETIC_CASE != collection-failure ]] || exit 7
   printf '%s' synthetic-recovered-root > "$OPENBAO_RECOVERY_DIR/root-token"
   if [[ $SYNTHETIC_CASE == collection-format ]]; then printf '%s\n' synthetic-private-input; else printf '%s\n' OPENBAO_BACKUP_INPUTS=PASS; fi;;
 image) printf '%s\n' synthetic-pinned-server;;
 *) exit 91;;
esac
`,
				"scripts/run-openbao-backup-sandbox.sh": `#!/bin/bash
set -eu
[[ $1 == verify && $2 == /dev/shm/email-custody-copy.* && $3 == "$RUNNER_TEMP/verify-openbao-recovery" ]] || exit 91
[[ $SYNTHETIC_CASE != restore-failure ]] || exit 7
if [[ $SYNTHETIC_CASE == restore-format ]]; then printf '%s\n' synthetic-private-restore; else
 touch "$SYNTHETIC_CASE_DIR/restored"
 printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3'
fi
`,
				"scripts/prepare-email-credentials.sh": `#!/bin/bash
set -eu
[[ -f $SYNTHETIC_CASE_DIR/restored && $EMAIL_CUSTODY_MODE == copy && $BAO_ADDR == http://127.0.0.1:38200 && $EMAIL_CUSTODY_TOKEN_FILE == "$EMAIL_CUSTODY_WORK_DIR/root-token" && $(cat "$EMAIL_CUSTODY_TOKEN_FILE") == synthetic-recovered-root ]] || exit 91
touch "$SYNTHETIC_CASE_DIR/copied"
case "$SYNTHETIC_CASE" in
 copy-failure) exit 7;;
 copy-partial) printf '%s\n' 'EMAIL_CUSTODY_PREPARATION=VERIFIED mode=copy existing=0 copied=2 pending=0 unseeded=0';;
 copy-private) printf '%s\n' synthetic-private-copy;;
 existing) printf '%s\n' 'EMAIL_CUSTODY_PREPARATION=VERIFIED mode=copy existing=3 copied=0 pending=0 unseeded=0';;
 mixed) printf '%s\n' 'EMAIL_CUSTODY_PREPARATION=VERIFIED mode=copy existing=1 copied=2 pending=0 unseeded=0';;
 *) printf '%s\n' 'EMAIL_CUSTODY_PREPARATION=VERIFIED mode=copy existing=0 copied=3 pending=0 unseeded=0';;
esac
`,
			}
			for name, body := range files {
				if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
					t.Fatal("could not prepare synthetic boundary")
				}
			}
			command := exec.Command("bash", "-eu", "-c", step.Run)
			command.Dir = dir
			command.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + dir, "RUNNER_TEMP=" + dir, "KUBE_CONFIG=synthetic-kubeconfig", "HCLOUD_TOKEN=synthetic-provider", "SYNTHETIC_CASE_DIR=" + dir, "SYNTHETIC_CASE=" + test.mode}
			out, err := command.CombinedOutput()
			if (err == nil) != test.allowed {
				t.Fatalf("copy accepted incomplete recovery or refused a complete proof: %v %s", err, out)
			}
			_, copyErr := os.Stat(filepath.Join(dir, "copied"))
			if (copyErr == nil) != test.copied {
				t.Fatal("copy happened without complete recovery or never followed successful recovery")
			}
			if strings.Contains(string(out), "synthetic-") {
				t.Fatal("public output exposed unvalidated private input")
			}
			if scratch, readErr := os.ReadFile(filepath.Join(dir, "scratch")); readErr == nil {
				if _, statErr := os.Stat(string(scratch)); !os.IsNotExist(statErr) {
					t.Fatal("private recovery scratch was not removed")
				}
			}
		})
	}
}
