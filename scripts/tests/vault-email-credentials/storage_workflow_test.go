package emailcredentials_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func storageKeySteps(t *testing.T, file, jobName string) []verificationStep {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(), ".github/workflows", file))
	if err != nil {
		t.Fatal("guarded storage-key workflow is missing")
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
	if yaml.Unmarshal(raw, &workflow) != nil || len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("storage key operation admits an ambient trigger or permission")
	}
	job, ok := workflow.Jobs[jobName]
	if !ok || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) < 3 {
		t.Fatal("storage key operation lost its protected boundary")
	}
	return job.Steps
}

func TestStoragePreparationRequiresIndependentRestoreBeforeSeeding(t *testing.T) {
	steps := storageKeySteps(t, "prepare-email-storage-key.yaml", "prepare")
	var step verificationStep
	for _, candidate := range steps {
		if candidate.Env["KUBE_CONFIG"] != "" {
			step = candidate
		}
	}
	if step.Run == "" || len(step.Env) != 3 || step.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || step.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || step.Env["EMAIL_STORAGE_CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("storage preparation admits an undeclared credential or confirmation")
	}
	for _, scenario := range []string{"create", "existing", "collection-failed", "restore-failed", "restore-format", "seed-failed", "seed-format"} {
		t.Run(scenario, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal("cannot resolve the fixture workspace")
			}
			for _, name := range []string{"bin", "scripts"} {
				if os.Mkdir(filepath.Join(dir, name), 0700) != nil {
					t.Fatal("cannot prepare the fixture workspace")
				}
			}
			files := map[string]string{
				"bin/findmnt": "#!/bin/sh\nprintf '%s\\n' tmpfs\n",
				"bin/mktemp":  "#!/bin/bash\n[[ $* == '-d /dev/shm/email-storage-key.XXXXXXXX' ]] || exit 91\nexec /usr/bin/mktemp -d \"$FIXTURE_DIR/private.XXXXXXXX\"\n",
				"bin/kubectl": "#!/bin/bash\n[[ $* == '--context=admin@prod -n openbao port-forward --address=127.0.0.1 service/openbao-active 38200:8200' ]] || exit 91\nexec /bin/sleep 30\n",
				"bin/curl":    "#!/bin/sh\nexit 0\n",
				"bin/docker":  "#!/bin/bash\n[[ $* == 'pull synthetic-pinned-server' ]] || exit 91\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/sh\nexit 0\n",
				"verify-openbao-recovery": `#!/bin/bash
set -eu
case "$*" in
 collect-email)
  [[ -s $KUBECONFIG && $KUBECONFIG != "$OPENBAO_RECOVERY_DIR/"* ]] || exit 91
  printf '%s' "$OPENBAO_RECOVERY_DIR" > "$FIXTURE_DIR/scratch"
  [[ $FIXTURE_CASE != collection-failed ]] || exit 7
  printf '%s' synthetic-recovered-root > "$OPENBAO_RECOVERY_DIR/root-token"
  printf '%s' synthetic-recovered-records > "$OPENBAO_RECOVERY_DIR/expected.json"
  printf '%s\n' OPENBAO_BACKUP_INPUTS=PASS;;
 image) printf '%s\n' synthetic-pinned-server;;
 seed-email-storage)
  [[ -f $FIXTURE_DIR/restored && $EMAIL_STORAGE_CONFIRM == prepare-email-storage-key && -s $OPENBAO_RECOVERY_DIR/root-token && -s $OPENBAO_RECOVERY_DIR/expected.json ]] || exit 91
  touch "$FIXTURE_DIR/seeded"
  [[ $FIXTURE_CASE != seed-failed ]] || exit 7
  if [[ $FIXTURE_CASE == seed-format ]]; then printf '%s\n' PRIVATE_SYNTHETIC_PAYLOAD
  elif [[ $FIXTURE_CASE == existing ]]; then printf '%s\n' 'EMAIL_STORAGE_KEY=PREPARED state=EXISTING recovered=false'
  else printf '%s\n' 'EMAIL_STORAGE_KEY=PREPARED state=CREATED recovered=false'; fi;;
 *) exit 91;;
esac
`,
				"scripts/run-openbao-backup-sandbox.sh": `#!/bin/bash
set -eu
[[ $1 == verify-email && $2 == "$OPENBAO_RECOVERY_DIR" && $3 == "$RUNNER_TEMP/verify-openbao-recovery" && ! -e $2/kubeconfig ]] || exit 91
[[ $FIXTURE_CASE != restore-failed ]] || exit 7
if [[ $FIXTURE_CASE == restore-format ]]; then printf '%s\n' PRIVATE_SYNTHETIC_PAYLOAD; else
 touch "$FIXTURE_DIR/restored"
 printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3'
fi
`,
			}
			for name, body := range files {
				if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
					t.Fatal("cannot prepare a fixture command")
				}
			}
			command := exec.Command("bash", "-eu", "-c", step.Run)
			command.Dir = dir
			command.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + dir, "KUBE_CONFIG=synthetic-kubeconfig", "HCLOUD_TOKEN=synthetic-provider", "EMAIL_STORAGE_CONFIRM=prepare-email-storage-key", "FIXTURE_DIR=" + dir, "FIXTURE_CASE=" + scenario}
			output, err := command.CombinedOutput()
			allowed := scenario == "create" || scenario == "existing"
			if (err == nil) != allowed || strings.Contains(string(output), "PRIVATE_SYNTHETIC") {
				t.Fatal("storage workflow accepted incomplete recovery or exposed private output")
			}
			_, seeded := os.Stat(filepath.Join(dir, "seeded"))
			wantSeed := allowed || scenario == "seed-failed" || scenario == "seed-format"
			if (seeded == nil) != wantSeed {
				t.Fatal("storage seed bypassed or never followed independent restoration")
			}
			if scratch, err := os.ReadFile(filepath.Join(dir, "scratch")); err == nil {
				if _, err := os.Stat(filepath.Dir(string(scratch))); !os.IsNotExist(err) {
					t.Fatal("storage workflow retained private connection or recovery inputs")
				}
			}
		})
	}
}

func TestStorageKeyWorkflowsRefuseUnconfirmedExecutionBeforeCheckout(t *testing.T) {
	for _, workflow := range []struct{ file, job, confirmation string }{
		{"prepare-email-storage-key.yaml", "prepare", "prepare-email-storage-key"},
		{"verify-email-storage-key-recovery.yaml", "verify", "verify-email-storage-key-recovery"},
	} {
		t.Run(workflow.job, func(t *testing.T) {
			guard := storageKeySteps(t, workflow.file, workflow.job)[0]
			if guard.Uses != "" || guard.Env["CONFIRM"] != "${{ inputs.confirm }}" || len(guard.Env) != 1 {
				t.Fatal("storage key confirmation must precede checkout and secret access")
			}
			valid := map[string]string{"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "CONFIRM": workflow.confirmation}
			for _, changed := range []string{"confirmed", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "CONFIRM"} {
				command := exec.Command("bash", "-eu", "-c", guard.Run)
				command.Env = []string{"PATH=" + os.Getenv("PATH")}
				for name, value := range valid {
					if name == changed {
						value = "unapproved"
					}
					command.Env = append(command.Env, name+"="+value)
				}
				if err := command.Run(); (err == nil) != (changed == "confirmed") {
					t.Fatal("storage key workflow admitted an unconfirmed execution")
				}
			}
		})
	}
}
