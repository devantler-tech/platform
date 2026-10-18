package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type repositoryGuardStep struct {
	Name     string            `yaml:"name"`
	If       string            `yaml:"if"`
	Run      string            `yaml:"run"`
	Continue any               `yaml:"continue-on-error"`
	Env      map[string]string `yaml:"env"`
}

type repositoryGuardJob struct {
	If          string                `yaml:"if"`
	Needs       []string              `yaml:"needs"`
	Continue    any                   `yaml:"continue-on-error"`
	Permissions map[string]string     `yaml:"permissions"`
	Steps       []repositoryGuardStep `yaml:"steps"`
}

type repositoryGuardWorkflow struct {
	Jobs map[string]repositoryGuardJob `yaml:"jobs"`
}

// readRepositoryGuardWorkflow examines the actual workflow consumed by Actions.
func readRepositoryGuardWorkflow(t *testing.T) repositoryGuardWorkflow {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow repositoryGuardWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

// repositoryGuardContract rejects dependencies that serialize classification or
// allow deployment and required checks to bypass unconditional repository guards.
func repositoryGuardContract(t *testing.T, workflow repositoryGuardWorkflow) {
	t.Helper()
	guards, ok := workflow.Jobs["validate-repository-guards"]
	if !ok {
		t.Fatal("missing independent repository guards")
	}
	if guards.If != "" || len(guards.Needs) != 0 || guards.Continue != nil {
		t.Fatal("repository guards must run independently and unconditionally")
	}
	if len(guards.Permissions) != 1 || guards.Permissions["contents"] != "read" {
		t.Fatal("repository guards need only read-only checkout access")
	}
	for _, job := range []string{"deploy-prod", "ci-required-checks"} {
		found := false
		for _, dependency := range workflow.Jobs[job].Needs {
			found = found || dependency == "validate-repository-guards"
		}
		if !found {
			t.Fatalf("%s can bypass repository guards", job)
		}
	}
	for _, dependency := range workflow.Jobs["changes"].Needs {
		if dependency == "validate-repository-guards" {
			t.Fatal("path classification must not wait for repository guards")
		}
	}
	if workflow.Jobs["ci-required-checks"].If != "always()" {
		t.Fatal("required-check evaluation must run after unsuccessful dependencies")
	}
}

// TestRepositoryGuardsIndependent verifies the execution boundary, not a copied
// inventory of filenames or the formatting of the workflow.
func TestRepositoryGuardsIndependent(t *testing.T) {
	workflow := readRepositoryGuardWorkflow(t)
	repositoryGuardContract(t, workflow)
	for _, step := range workflow.Jobs["validate-repository-guards"].Steps {
		if step.If != "" || step.Continue != nil {
			t.Fatalf("repository guard %q may not skip or suppress a failure", step.Name)
		}
		if strings.Contains(step.Run, "needs.changes") || strings.Contains(step.If, "needs.changes") {
			t.Fatalf("repository guard %q depends on classification", step.Name)
		}
	}
}

// TestRepositoryGuardsRequiredResult executes the workflow's real result guard.
// The generic aggregator tolerates legitimate skips elsewhere; this job may not.
func TestRepositoryGuardsRequiredResult(t *testing.T) {
	workflow := readRepositoryGuardWorkflow(t)
	repositoryGuardContract(t, workflow)
	var guard *repositoryGuardStep
	for _, step := range workflow.Jobs["ci-required-checks"].Steps {
		if step.Env["REPOSITORY_GUARDS_RESULT"] != "${{ needs.validate-repository-guards.result }}" {
			continue
		}
		if guard != nil {
			t.Fatal("duplicate repository result guard")
		}
		copy := step
		guard = &copy
	}
	if guard == nil || guard.If != "" || guard.Continue != nil || strings.TrimSpace(guard.Run) == "" {
		t.Fatal("missing unconditional repository result guard")
	}
	for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
		t.Run(result, func(t *testing.T) {
			command := exec.Command("bash", "-e", "-o", "pipefail", "-c", guard.Run)
			command.Env = append(os.Environ(), "REPOSITORY_GUARDS_RESULT="+result)
			output, err := command.CombinedOutput()
			if result == "success" && err != nil {
				t.Fatalf("successful repository guards rejected: %v\n%s", err, output)
			}
			if result != "success" && err == nil {
				t.Fatalf("accepted incomplete repository guards: %q", result)
			}
		})
	}
}
