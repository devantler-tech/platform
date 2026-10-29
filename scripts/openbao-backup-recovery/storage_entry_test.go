package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStorageSeedModeUsesOnlyTheRecoveredCanonicalAccounts(t *testing.T) {
	mode, paths, ok := credentialMode("seed-email-storage")
	want := []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent"}
	if !ok || mode != "seed-storage" || !slices.Equal(paths, want) {
		t.Fatal("seed mode did not bind the recovered account prerequisite")
	}
}

func TestStorageSeedEntrypointRefusesDispatchGuardsBeforePrivateReads(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "verifier")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build the owned entrypoint: %v %s", err, output)
	}
	valid := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": strings.Repeat("a", 40), "GITHUB_RUN_ATTEMPT": "1", "EMAIL_STORAGE_CONFIRM": "prepare-email-storage-key"}
	for _, changed := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_SHA", "GITHUB_RUN_ATTEMPT", "EMAIL_STORAGE_CONFIRM", "private-inputs"} {
		t.Run(changed, func(t *testing.T) {
			command := exec.Command(binary, "seed-email-storage")
			command.Env = append(os.Environ(), "OPENBAO_RECOVERY_DIR=/nonexistent-synthetic-storage-inputs")
			for name, value := range valid {
				if name == changed {
					value = "unapproved"
				}
				command.Env = append(command.Env, name+"="+value)
			}
			output, err := command.CombinedOutput()
			stage := "guard"
			if changed == "private-inputs" {
				stage = "inputs"
			}
			if err == nil || strings.TrimSpace(string(output)) != "EMAIL_STORAGE_KEY=UNVERIFIED stage="+stage {
				t.Fatal("entrypoint did not refuse before accessing private inputs")
			}
		})
	}
}
