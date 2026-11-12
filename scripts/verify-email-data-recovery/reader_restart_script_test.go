package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReaderRestartScriptPreservesIsolationAndRefusesIncompleteProof exercises the authored wrapper, not a source-text substitute.
func TestReaderRestartScriptPreservesIsolationAndRefusesIncompleteProof(t *testing.T) {
	t.Chdir("../..")
	script, err := os.ReadFile("scripts/prepare-retained-email-reader-witness.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"matching", "foreign-dispatch", "missing-witness", "collection", "preflight", "restore", "wrong-count", "changed-source", "uncertain-custody", "wrong-fingerprint", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin, runner := filepath.Join(dir, "bin"), filepath.Join(dir, "runner")
			for _, path := range []string{bin, filepath.Join(dir, "scripts"), filepath.Join(runner, "email-restart-witness-tools")} {
				if os.MkdirAll(path, 0700) != nil {
					t.Fatal("fixture directory")
				}
			}
			files := map[string]string{
				"bin/id":      "#!/bin/bash\nprintf '%s\\n' 1000\n",
				"bin/findmnt": "#!/bin/bash\nprintf '%s\\n' tmpfs\n",
				"bin/mktemp":  "#!/bin/bash\nexec /usr/bin/mktemp -d \"$FIXTURE_DIR/ram.XXXXXXXX\"\n",
				"bin/curl":    "#!/bin/bash\nprintf '%s\\n' '{\"initialized\":true,\"sealed\":false,\"standby\":false}'\n",
				"bin/docker":  "#!/bin/bash\n[[ $1 == pull ]]\n",
				"bin/rm":      "#!/bin/bash\n[[ $FIXTURE_CASE != cleanup ]] || exit 91\nexec /bin/rm \"$@\"\n",
				"bin/kubectl": `#!/bin/bash
set -eu
[[ $* == *' port-forward '* && $* == *'service/openbao-active 38200:8200'* ]] || exit 91
printf '%s' "$$" > "$FIXTURE_DIR/forward-pid"
exec /bin/sleep 60
`,
				"scripts/verify-prod-recovery-source.sh": `#!/bin/bash
set -eu
[[ $1 =~ ^[0-9a-f]{40}$ ]] || exit 91
count=0
[[ ! -f "$FIXTURE_DIR/source-count" ]] || count=$(cat "$FIXTURE_DIR/source-count")
count=$((count+1))
printf '%s' "$count" > "$FIXTURE_DIR/source-count"
[[ $FIXTURE_CASE != changed-source || $count == 1 ]] || exit 91
`,
				"scripts/use-prod-stable-api-endpoint.sh": `#!/bin/bash
printf '%s\n' NONSECRET_MASK_CANARY >&2
printf '%s' PRIVATE_SYNTHETIC_ENDPOINT
`,
				"runner/email-restart-witness-tools/recovery": `#!/bin/bash
set -eu
if [[ $* == image ]]; then printf '%s\n' synthetic-pinned-server; exit 0; fi
[[ $* == collect-email-retained-inputs && -s $KUBECONFIG ]] || exit 91
if [[ $FIXTURE_CASE == collection ]]; then printf '%s' PRIVATE_SYNTHETIC_CREDENTIAL >&2; exit 91; fi
for name in root-token unseal-key snapshot.snap expected.json; do printf '%s' synthetic > "$OPENBAO_RECOVERY_DIR/$name"; done
printf '%s\n' OPENBAO_BACKUP_INPUTS=PASS
`,
				"runner/email-restart-witness-tools/reader": `#!/bin/bash
set -eu
[[ -s $KUBECONFIG && -s $EMAIL_READER_INPUTS_WORK_DIR/token && -s $EMAIL_READER_INPUTS_WORK_DIR/expected.json ]] || exit 91
if [[ $* == --inspect-reader-witness ]]; then
  if [[ $FIXTURE_CASE == preflight ]]; then printf '%s' PRIVATE_SYNTHETIC_GRAPH >&2; exit 91; fi
  printf '%s' synthetic-private-witness > "$EMAIL_READER_INPUTS_WORK_DIR/restart-witness.json"
  printf '%s\n' 'EMAIL_READER_RESTART_WITNESS=PREFLIGHT source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED'
  exit 0
fi
[[ $* == --prepare-reader-witness && -s $EMAIL_READER_INPUTS_WORK_DIR/restart-witness.json && -s $EMAIL_READER_INPUTS_WORK_DIR/restore-result && -f $FIXTURE_DIR/sandbox-complete ]] || exit 91
printf '%s' 1 > "$FIXTURE_DIR/witness-post"
if [[ $FIXTURE_CASE == uncertain-custody ]]; then printf '%s' PRIVATE_UNCERTAIN_RESPONSE >&2; exit 91; fi
digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
[[ $FIXTURE_CASE != wrong-fingerprint ]] || digest=foreign
printf 'EMAIL_READER_RESTART_WITNESS=STORED record_sha256=%s source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED\n' "$digest"
`,
				"scripts/run-openbao-backup-sandbox.sh": `#!/bin/bash
set -eu
[[ $1 == verify-email-retained-inputs && -z ${KUBECONFIG:-} && -z ${EMAIL_READER_INPUTS_WORK_DIR:-} && -z ${OPENBAO_RECOVERY_DIR:-} && ! -e $2/token && ! -e $2/kubeconfig && ! -e $2/restart-witness.json ]] || exit 91
if kill -0 "$(cat "$FIXTURE_DIR/forward-pid")" 2>/dev/null; then exit 91; fi
for name in root-token unseal-key snapshot.snap expected.json; do [[ -s $2/$name ]] || exit 91; done
if [[ $FIXTURE_CASE == restore ]]; then printf '%s' PRIVATE_RESTORE_ERROR >&2; exit 91; fi
printf '%s' complete > "$FIXTURE_DIR/sandbox-complete"
if [[ $FIXTURE_CASE == wrong-count ]]; then printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3'; else printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3'; fi
`,
			}
			for name, body := range files {
				if os.WriteFile(filepath.Join(dir, name), []byte(body), 0700) != nil {
					t.Fatal("executable synthetic fixture")
				}
			}
			path := filepath.Join(dir, "prepare.sh")
			if os.WriteFile(path, script, 0600) != nil {
				t.Fatal("operator fixture")
			}
			env := readerRestartDispatch()
			if scenario == "foreign-dispatch" {
				env["GITHUB_RUN_ATTEMPT"] = "2"
			}
			if scenario == "missing-witness" {
				delete(env, "EMAIL_RETAINED_REPLICA_UIDS")
			}
			command := exec.Command("/bin/bash", "--noprofile", "--norc", path)
			command.Dir = dir
			command.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + runner, "KUBE_CONFIG=PRIVATE_SYNTHETIC_KUBECONFIG", "HCLOUD_TOKEN=PRIVATE_SYNTHETIC_PROVIDER", "FIXTURE_DIR=" + dir, "FIXTURE_CASE=" + scenario}
			for key, value := range env {
				command.Env = append(command.Env, key+"="+value)
			}
			output, err := command.CombinedOutput()
			if (err == nil) != (scenario == "matching") || strings.Contains(string(output), "WITNESS=STORED") != (scenario == "matching") || strings.Contains(string(output), "PRIVATE_") || strings.Contains(string(output), "restart=AUTHORIZED") {
				t.Fatalf("wrapper accepted incomplete recovery or exposed private state: case=%s output=%q", scenario, output)
			}
			_, posted := os.Stat(filepath.Join(dir, "witness-post"))
			if (scenario == "matching" || scenario == "uncertain-custody" || scenario == "wrong-fingerprint" || scenario == "cleanup") != (posted == nil) {
				t.Fatal("witness write was not ordered after restored-input/source proof")
			}
			if scenario == "foreign-dispatch" || scenario == "missing-witness" {
				stage := "dispatch"
				if scenario == "missing-witness" {
					stage = "independent-witnesses"
				}
				if strings.TrimSpace(string(output)) != "EMAIL_READER_RESTART_WITNESS=UNVERIFIED stage="+stage+" restart=NOT_AUTHORIZED recovery=UNVERIFIED" {
					t.Fatal("invalid dispatch did not refuse cleanly before private access")
				}
			}
		})
	}
}
