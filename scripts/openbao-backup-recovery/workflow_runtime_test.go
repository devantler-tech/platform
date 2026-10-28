package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRecoveryWorkflowExcludesClusterCredentials exercises the actual shell handoff.
func TestRecoveryWorkflowExcludesClusterCredentials(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the workflow requires a non-root caller")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "verify-openbao-backup-recovery.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Run string } }
	}
	if yaml.Unmarshal(raw, &workflow) != nil || len(workflow.Jobs["verify"].Steps) != 5 {
		t.Fatal("cannot read guarded recovery workflow")
	}
	body := workflow.Jobs["verify"].Steps[4].Run
	for _, tc := range []string{"matching", "collection-refused", "collection-stage", "private-collection-stage", "private-result", "restore-stage", "private-restore-stage", "changed-main", "missing-cluster-config"} {
		t.Run(tc, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			runner := filepath.Join(dir, "runner")
			for _, p := range []string{bin, filepath.Join(dir, "scripts"), filepath.Join(runner, "openbao-recovery-tools")} {
				if os.MkdirAll(p, 0700) != nil {
					t.Fatal("cannot prepare private fixture")
				}
			}
			files := map[string]string{
				"bin/git": `#!/bin/bash
set -eu
[[ $1 == ls-remote ]] || exit 91
if [[ $FIXTURE_CASE == changed-main ]]; then printf '%040d\trefs/heads/main\n' 2; else printf '%s\trefs/heads/main\n' "$GITHUB_SHA"; fi
`,
				"bin/findmnt": "#!/bin/sh\nprintf '%s\\n' tmpfs\n",
				"bin/mktemp": `#!/bin/bash
set -eu
[[ $1 == -d && $2 == /dev/shm/openbao-recovery.XXXXXXXX ]] || exit 91
exec /usr/bin/mktemp -d "$FIXTURE_DIR/proof.XXXXXXXX"
`,
				"bin/kubectl": `#!/bin/bash
set -eu
[[ $* == *port-forward* ]] || exit 91
exec /bin/sleep 30
`,
				"bin/curl":   "#!/bin/sh\nexit 0\n",
				"bin/docker": "#!/bin/sh\n[ \"$1\" = pull ] || exit 91\n",
				"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/sh\nexit 0\n",
				"runner/openbao-recovery-tools/verifier": `#!/bin/bash
set -eu
if [[ $1 == image ]]; then printf '%s\n' '` + serverImage + `'; exit 0; fi
[[ $1 == collect ]] || exit 91
printf '%s' "$OPENBAO_RECOVERY_DIR" > "$FIXTURE_DIR/proof-path"
for name in root-token unseal-key expected.json snapshot.snap; do printf '%s' synthetic > "$OPENBAO_RECOVERY_DIR/$name"; done
if [[ $FIXTURE_CASE == collection-refused ]]; then printf '%s\n' PRIVATE_SYNTHETIC_PAYLOAD; exit 91; fi
if [[ $FIXTURE_CASE == collection-stage ]]; then printf '%s\n' 'OPENBAO_BACKUP_INPUTS=UNVERIFIED stage=snapshot-download' >&2; exit 91; fi
if [[ $FIXTURE_CASE == private-collection-stage ]]; then printf '%s\n' 'OPENBAO_BACKUP_INPUTS=UNVERIFIED stage=PRIVATE_SYNTHETIC_PAYLOAD' >&2; exit 91; fi
printf '%s\n' OPENBAO_BACKUP_INPUTS=PASS
`,
				"scripts/run-openbao-backup-sandbox.sh": `#!/bin/bash
set -eu
printf '%s' reached > "$FIXTURE_DIR/sandbox-started"
[[ $1 == verify ]] || exit 91
if [[ -e $2/kubeconfig ]]; then printf '%s' exposed > "$FIXTURE_DIR/kubeconfig-exposed"; printf '%s\n' PRIVATE_SYNTHETIC_CLUSTER_CREDENTIAL >&2; exit 91; fi
for name in root-token unseal-key expected.json snapshot.snap; do [[ -s $2/$name ]] || exit 91; done
if [[ $FIXTURE_CASE == restore-stage ]]; then printf '%s\n' 'OPENBAO_BACKUP_RESTORE=FAIL stage=unseal'; exit 91; fi
if [[ $FIXTURE_CASE == private-restore-stage ]]; then printf '%s\n' 'OPENBAO_BACKUP_RESTORE=FAIL stage=PRIVATE_SYNTHETIC_PAYLOAD'; exit 91; fi
if [[ $FIXTURE_CASE == private-result ]]; then printf '%s\n' PRIVATE_SYNTHETIC_PAYLOAD; else printf '%s\n' 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3'; fi
`,
			}
			for name, text := range files {
				if os.WriteFile(filepath.Join(dir, name), []byte(text), 0700) != nil {
					t.Fatal("cannot write private fixture tool")
				}
			}
			command := exec.Command("bash", "-c", body)
			command.Dir = dir
			command.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + runner, "GITHUB_SHA=" + strings.Repeat("1", 40), "KUBE_CONFIG=SYNTHETIC_KUBECONFIG", "HCLOUD_TOKEN=SYNTHETIC_PROVIDER_VALUE", "FIXTURE_DIR=" + dir, "FIXTURE_CASE=" + tc}
			if tc == "missing-cluster-config" {
				command.Env = append(command.Env, "KUBE_CONFIG=")
			}
			out, runErr := command.CombinedOutput()
			if (runErr == nil) != (tc == "matching") {
				if _, err := os.Stat(filepath.Join(dir, "kubeconfig-exposed")); err == nil {
					t.Fatal("sandbox received an unintended cluster credential")
				}
				t.Fatal("incorrect private handoff verdict", runErr)
			}
			if strings.Contains(string(out), "PRIVATE_SYNTHETIC") || strings.Contains(string(out), "SYNTHETIC_KUBECONFIG") || strings.Contains(string(out), "SYNTHETIC_PROVIDER") {
				t.Fatal("workflow exposed private fixture content")
			}
			if tc == "matching" && strings.TrimSpace(string(out)) != "OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3" {
				t.Fatal("workflow accepted an unverified result")
			}
			if tc == "collection-stage" && !strings.Contains(string(out), "OPENBAO_BACKUP_INPUTS=UNVERIFIED stage=snapshot-download") {
				t.Fatal("workflow hid the safe collection failure stage")
			}
			if tc == "restore-stage" && !strings.Contains(string(out), "OPENBAO_BACKUP_RESTORE=FAIL stage=unseal") {
				t.Fatal("workflow hid the safe restoration failure stage")
			}
			if tc != "matching" && !strings.Contains(string(out), "OPENBAO_BACKUP_STAGE=") {
				t.Fatal("workflow did not identify its bounded phase of refusal")
			}
			if tc == "missing-cluster-config" && !strings.Contains(string(out), "OPENBAO_BACKUP_STAGE=private-inputs") {
				t.Fatal("missing private input was mislabeled as a source failure")
			}
			proof, readErr := os.ReadFile(filepath.Join(dir, "proof-path"))
			if readErr == nil {
				if _, statErr := os.Stat(string(proof)); !os.IsNotExist(statErr) {
					t.Fatal("workflow retained its private scratch directory")
				}
			}
			_, sandboxErr := os.Stat(filepath.Join(dir, "sandbox-started"))
			if (sandboxErr == nil) != (tc == "matching" || tc == "private-result" || tc == "restore-stage" || tc == "private-restore-stage") {
				t.Fatal("workflow reached sandbox after a refused collection or stale main")
			}
		})
	}
}
