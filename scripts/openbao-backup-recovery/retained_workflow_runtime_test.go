package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRetainedInputRecoveryWorkflowRefusesForeignDispatch executes the authored pre-credential guard.
func TestRetainedInputRecoveryWorkflowRefusesForeignDispatch(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/verify-retained-email-reader-inputs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		On          map[string]any `yaml:"on"`
		Permissions map[string]string
		Concurrency struct {
			Group, Queue string
			Cancel       bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			Environment string
			Timeout     int `yaml:"timeout-minutes"`
			Permissions map[string]string
			Steps       []struct {
				Run  string
				Env  map[string]string
				With map[string]any
			}
		}
	}
	if yaml.Unmarshal(raw, &w) != nil || len(w.On) != 1 || w.On["workflow_dispatch"] == nil || len(w.Permissions) != 0 || w.Concurrency.Group != "prod-deploy" || w.Concurrency.Queue != "max" || w.Concurrency.Cancel || len(w.Jobs) != 1 {
		t.Fatal("recovery workflow widened its permission/trigger/serialization boundary")
	}
	job := w.Jobs["verify"]
	if len(job.Steps) != 6 || job.Environment != "prod" || job.Timeout != 25 || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("workflow lost its bounded protected runtime")
	}
	for _, key := range []string{"", "GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF", "GITHUB_ACTOR", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "CONFIRM"} {
		env := retainedRecoveryWorkflowEnvironment()
		env["CONFIRM"] = "verify-retained-email-reader-inputs"
		if key != "" {
			env[key] = "foreign"
		}
		cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", job.Steps[0].Run+"\nprintf ADMITTED")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if (err == nil) != (key == "") || strings.Contains(string(out), "ADMITTED") != (key == "") {
			t.Fatal("authored workflow admitted foreign dispatch or refused the confirmed operator")
		}
	}
	if job.Steps[1].With["ref"] != "${{ github.sha }}" || job.Steps[1].With["persist-credentials"] != false {
		t.Fatal("recovery source was not immutable and credential free")
	}
	for _, step := range job.Steps[:5] {
		if step.Env["KUBE_CONFIG"] != "" || step.Env["HCLOUD_TOKEN"] != "" {
			t.Fatal("production credentials preceded command builds")
		}
	}
	last := job.Steps[5]
	if last.Run != "bash scripts/verify-retained-email-reader-inputs.sh" || last.Env["EMAIL_RETAINED_READER_UID"] != "${{ secrets.EMAIL_RETAINED_READER_UID }}" || last.Env["EMAIL_RETAINED_CLAIM_UID"] != "${{ secrets.EMAIL_RETAINED_CLAIM_UID }}" || last.Env["EMAIL_READER_INPUTS_CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("workflow lost fixed entrypoint or independent identity witnesses")
	}
}

// retainedRecoveryWorkflowEnvironment contains only nonsecret synthetic first-attempt dispatch facts.
func retainedRecoveryWorkflowEnvironment() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_ACTOR": "devantler", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/verify-retained-email-reader-inputs.yaml@refs/heads/main", "EMAIL_READER_INPUTS_CONFIRM": "verify-retained-email-reader-inputs", "GITHUB_SHA": strings.Repeat("1", 40)}
}

// TestRetainedInputRecoveryHandoffPreservesTheReadOnlyBoundary exercises the actual operator shell around external services.
func TestRetainedInputRecoveryHandoffPreservesTheReadOnlyBoundary(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the actual operator requires a non-root runner")
	}
	script, err := filepath.Abs("../verify-retained-email-reader-inputs.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"matching", "foreign-dispatch", "pre-parity", "collection", "restore", "wrong-count", "post-source", "post-parity", "private-result"} {
		t.Run(scenario, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			runner := filepath.Join(dir, "runner")
			for _, path := range []string{bin, filepath.Join(dir, "scripts"), filepath.Join(runner, "email-input-recovery-tools")} {
				if os.MkdirAll(path, 0700) != nil {
					t.Fatal("fixture directory")
				}
			}
			files := map[string]string{
				"bin/findmnt": "#!/bin/sh\nprintf '%s\\n' tmpfs\n",
				"bin/mktemp": `#!/bin/bash
set -eu
[[ $1 == -d ]] || exit 91
case "$2" in /dev/shm/email-input-native.XXXXXXXX) kind=native ;; /dev/shm/email-input-proof.XXXXXXXX) kind=proof ;; *) exit 91 ;; esac
dir=$(/usr/bin/mktemp -d "$FIXTURE_DIR/$kind.XXXXXXXX")
printf '%s' "$dir" > "$FIXTURE_DIR/$kind-path"
printf '%s\n' "$dir"
`,
				"bin/kubectl": `#!/bin/bash
set -eu
case "$*" in
  *"port-forward"*) printf '%s' "$$" > "$FIXTURE_DIR/forward-pid"; exec /bin/sleep 30 ;;
  *) exit 91 ;;
esac
`,
				"bin/curl":   "#!/bin/sh\nprintf '%s' '{\"initialized\":true,\"sealed\":false,\"standby\":false}'\n",
				"bin/docker": "#!/bin/sh\n[ \"$1\" = pull ] || exit 91\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/sh\nprintf '%s\\n' NONSECRET_MASK_CANARY >&2\n",
				"scripts/verify-prod-recovery-source.sh": `#!/bin/bash
set -eu
count=0
[[ ! -e "$FIXTURE_DIR/source-count" ]] || count=$(cat "$FIXTURE_DIR/source-count")
count=$((count+1)); printf '%s' "$count" > "$FIXTURE_DIR/source-count"
if [[ $FIXTURE_CASE == post-source && $count == 2 ]]; then exit 91; fi
`,
				"runner/email-input-recovery-tools/reader": `#!/bin/bash
set -eu
[[ $* == --verify-reader-inputs && -s $KUBECONFIG && -s $EMAIL_READER_INPUTS_WORK_DIR/token && -s $EMAIL_READER_INPUTS_WORK_DIR/expected.json ]] || exit 91
count=0
[[ ! -e "$FIXTURE_DIR/parity-count" ]] || count=$(cat "$FIXTURE_DIR/parity-count")
count=$((count+1)); printf '%s' "$count" > "$FIXTURE_DIR/parity-count"
if [[ ($FIXTURE_CASE == pre-parity && $count == 1) || ($FIXTURE_CASE == post-parity && $count == 2) ]]; then printf '%s' PRIVATE_SYNTHETIC_VALUE >&2; exit 91; fi
if [[ $FIXTURE_CASE == private-result ]]; then printf '%s' PRIVATE_SYNTHETIC_VALUE; else printf '%s\n' 'EMAIL_READER_INPUT_PARITY=VERIFIED source_writes=0 restart=NOT_AUTHORIZED recovery=UNVERIFIED'; fi
`,
				"runner/email-input-recovery-tools/recovery": `#!/bin/bash
set -eu
if [[ $* == image ]]; then printf '%s\n' '` + serverImage + `'; exit 0; fi
[[ $* == collect-email-retained-inputs && -s $KUBECONFIG && $OPENBAO_RECOVERY_DIR != "${KUBECONFIG%/*}" ]] || exit 91
if [[ $FIXTURE_CASE == collection ]]; then printf '%s' PRIVATE_SYNTHETIC_VALUE >&2; exit 91; fi
for name in root-token unseal-key snapshot.snap expected.json; do printf '%s' synthetic > "$OPENBAO_RECOVERY_DIR/$name"; done
printf '%s\n' OPENBAO_BACKUP_INPUTS=PASS
`,
				"scripts/run-openbao-backup-sandbox.sh": `#!/bin/bash
set -eu
[[ $1 == verify-email-retained-inputs && -z ${KUBECONFIG:-} && -z ${EMAIL_READER_INPUTS_WORK_DIR:-} && -z ${OPENBAO_RECOVERY_DIR:-} && ! -e $2/kubeconfig && ! -e $2/token ]] || exit 91
if kill -0 "$(cat "$FIXTURE_DIR/forward-pid")" 2>/dev/null; then exit 91; fi
for name in root-token unseal-key snapshot.snap expected.json; do [[ -s $2/$name ]] || exit 91; done
printf '%s' reached > "$FIXTURE_DIR/sandbox-started"
if [[ $FIXTURE_CASE == restore ]]; then printf '%s' PRIVATE_SYNTHETIC_VALUE >&2; exit 91; fi
if [[ $FIXTURE_CASE == wrong-count ]]; then printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3'; else printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3'; fi
`,
			}
			for name, body := range files {
				if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
					t.Fatal("fixture executable")
				}
			}
			cmd := exec.Command("/bin/bash", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + runner, "KUBE_CONFIG=PRIVATE_SYNTHETIC_KUBECONFIG", "HCLOUD_TOKEN=PRIVATE_SYNTHETIC_PROVIDER", "FIXTURE_DIR=" + dir, "FIXTURE_CASE=" + scenario}
			for key, value := range retainedRecoveryWorkflowEnvironment() {
				if scenario == "foreign-dispatch" && key == "GITHUB_RUN_ATTEMPT" {
					value = "2"
				}
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "matching") || strings.Contains(string(out), "INPUT_RECOVERY=VERIFIED") != (scenario == "matching") {
				t.Fatalf("operator accepted incomplete evidence or refused a complete handoff: case=%s output=%q", scenario, string(out))
			}
			if strings.Contains(string(out), "PRIVATE_SYNTHETIC") {
				t.Fatal("operator exposed private service output or credentials")
			}
			if scenario != "foreign-dispatch" && !strings.Contains(string(out), "NONSECRET_MASK_CANARY") {
				t.Fatal("operator swallowed endpoint masking stream")
			}
			for _, kind := range []string{"native", "proof"} {
				if b, err := os.ReadFile(filepath.Join(dir, kind+"-path")); err == nil {
					if _, err = os.Stat(string(b)); !os.IsNotExist(err) {
						t.Fatal("owned private workspace retained after completion")
					}
				}
			}
			if scenario == "matching" {
				b, err := os.ReadFile(filepath.Join(dir, "parity-count"))
				if err != nil || string(b) != "2" {
					t.Fatal("operator skipped fresh parity around the actual restore handoff")
				}
			}
		})
	}
}
