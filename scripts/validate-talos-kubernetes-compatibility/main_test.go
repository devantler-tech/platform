package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func fixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ksail.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersionPairings(t *testing.T) {
	for _, tc := range []struct{ name, talos, kubernetes, diagnostic string }{
		{"current production", "v1.13.9", "v1.36.4", ""},
		{"in-range patch bump", "v1.13.9", "v1.36.5", ""},
		{"rejected PR 3534", "v1.13.9", "v1.37.0", "too new"},
		{"too old", "v1.13.9", "v1.30.0", "too old"},
		{"unknown Talos release", "v99.0.0", "v1.36.4", "not supported"},
		{"missing Talos pin", "", "v1.36.4", "explicit"},
		{"missing Kubernetes pin", "v1.13.9", "", "explicit"},
		{"malformed Talos pin", "latest", "v1.36.4", "explicit"},
		{"malformed Kubernetes pin", "v1.13.9", "v1.36", "explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "spec:\n  cluster:\n    kubernetesVersion: " + tc.kubernetes + "\n    talos:\n      version: " + tc.talos + "\n"
			err := validate(fixture(t, body))
			if tc.diagnostic == "" {
				if err != nil {
					t.Fatalf("compatible pairing rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("expected %q rejection, got %v", tc.diagnostic, err)
			}
			if tc.name == "rejected PR 3534" {
				for _, text := range []string{"v1.13.9", "v1.37.0", "Talos upgrade"} {
					if !strings.Contains(err.Error(), text) {
						t.Errorf("diagnostic does not name %q: %v", text, err)
					}
				}
			}
		})
	}
}

func TestMalformedConfigurationFailsClosed(t *testing.T) {
	for _, body := range []string{
		"",
		"spec: [",
		"spec: {}\nspec: {}\n",
		"spec:\n  cluster:\n    kubernetesVersion: v1.36.4\n    talos:\n      version: v1.13.9\n---\nspec: {}\n",
	} {
		if err := validate(fixture(t, body)); err == nil {
			t.Errorf("accepted malformed or incomplete config %q", body)
		}
	}
	if err := validate(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("accepted unreadable config")
	}
}

// Removing a version input from the actual path-filter registration must not
// turn a rejected pin into a skipped required job.
func TestProductionPinChangesSelectTalosValidation(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string            `yaml:"id"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var filters map[string][]string
	for _, step := range workflow.Jobs["changes"].Steps {
		if step.ID == "filter" {
			if err := yaml.Unmarshal([]byte(step.With["filters"]), &filters); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, changed := range []string{"ksail.prod.yaml", "go.mod", "go.sum", ".github/scripts/setup-talosctl.sh", "scripts/validate-talos-kubernetes-compatibility/main.go"} {
		matched := false
		for _, pattern := range filters["talos"] {
			ok, err := filepath.Match(pattern, changed)
			if err != nil {
				t.Fatal(err)
			}
			matched = matched || ok
		}
		if !matched {
			t.Errorf("change to %s skips required Talos validation", changed)
		}
	}
}

type workflowJob struct {
	If    string   `yaml:"if"`
	Needs []string `yaml:"needs"`
	Steps []struct {
		Uses string            `yaml:"uses"`
		With map[string]string `yaml:"with"`
		Run  string            `yaml:"run"`
	} `yaml:"steps"`
}

func repositoryWorkflow(t *testing.T, name string) map[string]workflowJob {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]workflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs
}

func jobRunsCompatibilityValidator(job workflowJob) bool {
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "go run ./scripts/validate-talos-kubernetes-compatibility ksail.prod.yaml") {
			return true
		}
	}
	return false
}

func jobNeeds(job workflowJob, dependency string) bool {
	for _, need := range job.Needs {
		if need == dependency {
			return true
		}
	}
	return false
}

func TestMergeGroupDeployRequiresCompatibilityValidation(t *testing.T) {
	jobs := repositoryWorkflow(t, "ci.yaml")
	validator, ok := jobs["validate-talos"]
	if !ok || !jobRunsCompatibilityValidator(validator) {
		t.Fatal("merge-queue workflow does not run the production Kubernetes/Talos compatibility validator")
	}
	if !strings.Contains(validator.If, "github.event_name == 'merge_group'") {
		t.Errorf("compatibility validator does not run on merge_group: if=%q", validator.If)
	}
	if !jobNeeds(jobs["deploy-prod"], "validate-talos") {
		t.Error("merge-group production deploy does not require compatibility validation")
	}
}

func TestManualDeployRequiresCompatibilityValidation(t *testing.T) {
	jobs := repositoryWorkflow(t, "cd.yaml")
	validator, ok := jobs["validate-talos-kubernetes-compatibility"]
	if !ok || !jobRunsCompatibilityValidator(validator) {
		t.Fatal("manual deployment workflow does not run the production Kubernetes/Talos compatibility validator")
	}
	if !jobNeeds(jobs["deploy-prod"], "validate-talos-kubernetes-compatibility") {
		t.Error("manual production deploy does not require compatibility validation")
	}
}

func TestHealDeployValidatesCheckedOutMain(t *testing.T) {
	steps := repositoryWorkflow(t, "ci.yaml")["heal-prod-on-failure"].Steps
	checkoutMain, validator, deploy := -1, -1, -1
	for i, step := range steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["ref"] == "main" {
			checkoutMain = i
		}
		if strings.Contains(step.Run, "go run ./scripts/validate-talos-kubernetes-compatibility ksail.prod.yaml") {
			validator = i
		}
		if step.Uses == "./.github/actions/deploy-prod" {
			deploy = i
		}
	}
	if checkoutMain < 0 {
		t.Fatal("heal route does not check out current main")
	}
	if validator < 0 {
		t.Fatal("heal route does not validate the checked-out main Kubernetes/Talos pairing")
	}
	if deploy < 0 {
		t.Fatal("heal route does not deploy production")
	}
	if !(checkoutMain < validator && validator < deploy) {
		t.Errorf("heal route must validate after checkout and before deploy: checkout=%d validator=%d deploy=%d", checkoutMain, validator, deploy)
	}
}
