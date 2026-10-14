package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type storageWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group  string `yaml:"group"`
		Cancel bool   `yaml:"cancel-in-progress"`
		Queue  string `yaml:"queue"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Environment map[string]any    `yaml:"environment"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Steps       []struct {
			Name, Run, Uses, If string
			Env                 map[string]string
			With                map[string]any
		}
	} `yaml:"jobs"`
}

// TestStorageWorkflowProtectsCredentialsAndExecutesDispatchGuard runs the actual precredential guard and verifies protected least-privilege ordering.
func TestStorageWorkflowProtectsCredentialsAndExecutesDispatchGuard(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/protect-backstage-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w storageWorkflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.On) != 1 || w.On["workflow_dispatch"] == nil || len(w.Permissions) != 0 || w.Concurrency.Group != "prod-deploy" || w.Concurrency.Cancel || w.Concurrency.Queue != "max" || len(w.Jobs) != 1 {
		t.Fatal("storage mutation must be separately confirmed, protected and serialized")
	}
	j := w.Jobs["protect"]
	if !reflect.DeepEqual(j.Environment, map[string]any{"name": "prod"}) || !reflect.DeepEqual(j.Permissions, map[string]string{"contents": "read", "actions": "read", "deployments": "read"}) || len(j.Steps) < 5 || j.Steps[0].Run == "" {
		t.Fatal("missing protected job or precredential guard")
	}
	credentialStep, sourceStep, executeStep := -1, -1, -1
	for i, step := range j.Steps {
		if strings.Contains(step.Run, "verify-prod-recovery-source.sh") {
			sourceStep = i
		}
		if _, ok := step.Env["KUBE_CONFIG"]; ok {
			credentialStep = i
		}
		if strings.Contains(step.Run, "go run ./scripts/protect-backstage-storage --execute") {
			executeStep = i
		}
		if strings.HasPrefix(step.Uses, "actions/checkout@") && (step.With["ref"] != "${{ github.sha }}" || step.With["persist-credentials"] != false) {
			t.Fatal("checkout is not pinned to the dispatch source")
		}
	}
	if sourceStep <= 0 || credentialStep <= sourceStep || executeStep <= credentialStep {
		t.Fatal("credentials or mutation precede the current-main proof")
	}
	good := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform",
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/protect-backstage-storage.yaml@refs/heads/main",
		"GITHUB_RUN_ATTEMPT":  "1", "GITHUB_RUN_ID": "42", "GITHUB_SHA": strings.Repeat("a", 40),
		"PORTAL_STORAGE_CONFIRM":        "PROTECT BACKSTAGE STORAGE ONLY",
		"PORTAL_STORAGE_REVISION":       "latest@sha256:" + strings.Repeat("b", 64),
		"PORTAL_STORAGE_DEPLOYMENT_RUN": "100",
	}
	for name, wrong := range map[string]string{
		"positive": "", "GITHUB_ACTIONS": "false", "GITHUB_REPOSITORY": "another/platform",
		"GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/branch",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/retire-backstage-stateless.yaml@refs/heads/main",
		"GITHUB_RUN_ATTEMPT":  "2", "GITHUB_RUN_ID": "", "GITHUB_SHA": "not-a-commit",
		"PORTAL_STORAGE_CONFIRM": "RETIRE BACKSTAGE STATELESS ONLY", "PORTAL_STORAGE_REVISION": "latest",
		"PORTAL_STORAGE_DEPLOYMENT_RUN": "not-a-run",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", j.Steps[0].Run)
			for key, value := range good {
				if key == name {
					value = wrong
				}
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != (name == "positive") {
				t.Fatalf("dispatch guard result err=%v output=%s", err, output)
			}
		})
	}
}

// TestStorageNativeControlsAreRequiredInCIAndBeforeCredentials prevents either caller from silently skipping the native transport controls.
func TestStorageNativeControlsAreRequiredInCIAndBeforeCredentials(t *testing.T) {
	for _, file := range []string{"ci.yaml", "protect-backstage-storage.yaml"} {
		t.Run(file, func(t *testing.T) {
			b, err := os.ReadFile("../../.github/workflows/" + file)
			if err != nil {
				t.Fatal(err)
			}
			var w map[string]any
			if err := yaml.Unmarshal(b, &w); err != nil {
				t.Fatal(err)
			}
			job := "protect"
			if file == "ci.yaml" {
				job = "changes"
			}
			checked := false
			for _, value := range array(field(w, "jobs", job, "steps")) {
				step := obj(value)
				if field(step, "env", "KUBE_CONFIG") != nil && !checked {
					t.Fatal("credentials precede required native controls")
				}
				if strings.Contains(str(step["run"]), "go test -race -count=1 ./internal/portalstorage ./scripts/protect-backstage-storage") {
					if field(step, "env", "PORTAL_STORAGE_REQUIRE_NATIVE_CONTROL") != "1" {
						t.Fatal("native patch control can be skipped")
					}
					checked = true
				}
			}
			if !checked {
				t.Fatal("required native patch control is absent")
			}
		})
	}
}

// TestRegistryProofUsesExistingProtectedPullCredentialOnlyAfterControls keeps registry access isolated from GitHub API identity and publication credentials.
func TestRegistryProofUsesExistingProtectedPullCredentialOnlyAfterControls(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/protect-backstage-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w storageWorkflow
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	steps := w.Jobs["protect"].Steps
	if w.Jobs["protect"].Env["GH_TOKEN"] != "${{ github.token }}" || w.Jobs["protect"].Env["DOCKER_CONFIG"] != "" {
		t.Fatal("GitHub API reads must keep the job's read-only token")
	}
	if w.Jobs["protect"].Permissions["packages"] != "" {
		t.Fatal("registry proof must not require broader repository package access")
	}
	controlled, authenticated := false, false
	for _, step := range steps {
		if strings.Contains(step.Run, "go test -race -count=1 ./internal/portalstorage ./scripts/protect-backstage-storage") {
			controlled = true
		}
		for _, value := range step.Env {
			if strings.Contains(value, "secrets.GHCR_TOKEN") {
				t.Fatal("publication credential must not enter the proof job")
			}
		}
		if _, ok := step.Env["SOPS_AGE_KEY"]; ok {
			if step.Env["DOCKER_CONFIG"] != "${{ runner.temp }}/portal-storage-registry" {
				t.Fatal("login must use the isolated step-scoped credential store")
			}
			if !controlled || step.Env["SOPS_AGE_KEY"] != "${{ secrets.SOPS_AGE_KEY }}" || !strings.Contains(step.Run, "decrypt_flux_ghcr_docker_config") {
				t.Fatal("unbound registry credential or credentials before controls")
			}
			authenticated = true
		}
		if strings.Contains(step.Run, "go run ./scripts/protect-backstage-storage --execute") {
			if step.Env["DOCKER_CONFIG"] != "${{ runner.temp }}/portal-storage-registry" {
				t.Fatal("OCI verification must read the same isolated credential store")
			}
			if !authenticated || step.Env["PORTAL_STORAGE_GHCR_READER_TOKEN"] != "" || step.Env["GH_TOKEN"] != "" {
				t.Fatal("registry-only attestation proof has no established reader authentication")
			}
		}
	}
	if !authenticated {
		t.Fatal("registry-only provenance reader lacks authentication")
	}
}

// TestProtectedRegistryStageRejectsMalformedCredentialAndCleansOnlyItsStore runs the real scalar-selection boundary with synthetic secrets and verifies bounded cleanup.
func TestProtectedRegistryStageRejectsMalformedCredentialAndCleansOnlyItsStore(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/protect-backstage-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w storageWorkflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	stage, cleanup := "", ""
	for _, step := range w.Jobs["protect"].Steps {
		if step.Env["SOPS_AGE_KEY"] != "" {
			stage = step.Run
		}
		if step.If == "always()" && step.Env["DOCKER_CONFIG"] == "${{ runner.temp }}/portal-storage-registry" {
			cleanup = step.Run
		}
	}
	if stage == "" || cleanup == "" {
		t.Fatal("missing protected credential staging or unconditional cleanup")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, config string
		ok           bool
	}{
		{"valid", `{"auths":{"ghcr.io":{"username":"reader","password":"private-test-value"}}}`, true},
		{"empty", `{}`, false},
		{"conflicting", `{"auths":{"ghcr.io":{"username":"reader","password":"private-test-value","auth":"b3RoZXI6dG9rZW4="}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			// Keep the real shared scalar-selection helper. Substitute only its
			// credential-decryption boundary; no live secret is accessed here.
			fake := "#!/bin/bash\nset -euo pipefail\n[[ $# == 8 && $1 == workload && $2 == cipher && $3 == decrypt && $4 == k8s/bases/bootstrap/secret.enc.yaml && $5 == --extract && $6 == '[\"stringData\"][\"ghcr_dockerconfigjson\"]' && $7 == --output ]]\nprintf '%s' \"$READER_CONFIG_FIXTURE\" > \"$8\"\nprintf '%s' \"$READER_CONFIG_FIXTURE\"\n"
			if err := os.WriteFile(filepath.Join(bin, "ksail"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			configDir := filepath.Join(dir, "portal-storage-registry")
			outside := filepath.Join(dir, "unrelated")
			if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			run := func(script string) ([]byte, error) {
				cmd := exec.Command("bash", "-c", script)
				cmd.Dir = root
				cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "DOCKER_CONFIG=" + configDir, "SOPS_AGE_KEY=not-a-real-key", "READER_CONFIG_FIXTURE=" + tc.config}
				return cmd.CombinedOutput()
			}
			output, err := run(stage)
			if (err == nil) != tc.ok {
				t.Fatalf("credential stage accepted=%v, want %v; output=%s", err == nil, tc.ok, output)
			}
			if strings.Contains(string(output), "private-test-value") || strings.Contains(string(output), tc.config) {
				t.Fatal("credential reached the stage log")
			}
			if tc.ok {
				info, err := os.Stat(filepath.Join(configDir, "config.json"))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("credential store is not private")
				}
			}
			if output, err := run(cleanup); err != nil {
				t.Fatalf("cleanup failed: %v %s", err, output)
			}
			if _, err := os.Stat(configDir); !os.IsNotExist(err) {
				t.Fatal("temporary credential store survived cleanup")
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "preserve" {
				t.Fatal("cleanup touched unrelated runner data")
			}
		})
	}
}
