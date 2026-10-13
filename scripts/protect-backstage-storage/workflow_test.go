package main

import (
	"os"
	"os/exec"
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
			Name, Run, Uses string
			Env             map[string]string
			With            map[string]any
		}
	} `yaml:"jobs"`
}

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
	if !reflect.DeepEqual(j.Environment, map[string]any{"name": "prod", "deployment": false}) || !reflect.DeepEqual(j.Permissions, map[string]string{"contents": "read", "actions": "read", "deployments": "read"}) || len(j.Steps) < 5 || j.Steps[0].Run == "" {
		t.Fatal("missing protected job or precredential guard")
	}
	credentialStep, sourceStep := -1, -1
	for i, step := range j.Steps {
		if strings.Contains(step.Run, "verify-prod-recovery-source.sh") {
			sourceStep = i
		}
		if _, ok := step.Env["KUBE_CONFIG"]; ok {
			credentialStep = i
		}
		if strings.HasPrefix(step.Uses, "actions/checkout@") && (step.With["ref"] != "${{ github.sha }}" || step.With["persist-credentials"] != false) {
			t.Fatal("checkout is not pinned to the dispatch source")
		}
	}
	if sourceStep <= 0 || credentialStep <= sourceStep || !strings.Contains(j.Steps[len(j.Steps)-1].Run, "go run ./scripts/protect-backstage-storage --execute") {
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

func TestRegistryProofUsesExistingProtectedCredentialOnlyAfterControls(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/protect-backstage-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w storageWorkflow
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	steps := w.Jobs["protect"].Steps
	if w.Jobs["protect"].Env["GH_TOKEN"] != "${{ github.token }}" {
		t.Fatal("GitHub API reads must keep the job's read-only token")
	}
	controlled, authenticated := false, false
	for _, step := range steps {
		if strings.Contains(step.Run, "go test -race -count=1 ./internal/portalstorage ./scripts/protect-backstage-storage") {
			controlled = true
		}
		if _, ok := step.Env["GHCR_TOKEN"]; ok {
			if !controlled || step.Env["GHCR_TOKEN"] != "${{ secrets.GHCR_TOKEN }}" || !strings.Contains(step.Run, "docker login ghcr.io") || !strings.Contains(step.Run, "--password-stdin") {
				t.Fatal("unbound registry credential or credentials before controls")
			}
			authenticated = true
		}
		if strings.Contains(step.Run, "go run ./scripts/protect-backstage-storage --execute") {
			if !authenticated || step.Env["PORTAL_STORAGE_GHCR_READER_TOKEN"] != "${{ secrets.GHCR_TOKEN }}" || step.Env["GH_TOKEN"] != "" {
				t.Fatal("registry-only attestation proof has no established reader authentication")
			}
		}
	}
	if !authenticated {
		t.Fatal("registry-only provenance reader lacks authentication")
	}
}
