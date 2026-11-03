package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMegaLinterKeepsNativePackageStyleEnabled(t *testing.T) {
	data, err := os.ReadFile("../../.mega-linter.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Executable []string `yaml:"GO_REVIVE_CLI_EXECUTABLE"`
		Disabled   []string `yaml:"DISABLE_LINTERS"`
		Advisory   []string `yaml:"DISABLE_ERRORS_LINTERS"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Executable, []string{"bash", "scripts/revive-package-context.sh"}) || slices.Contains(config.Disabled, "GO_REVIVE") || slices.Contains(config.Advisory, "GO_REVIVE") {
		t.Fatal("native Revive must remain a hard gate through the complete package adapter")
	}
}

func TestMegaLinterRequiresNativeFixtureRefusals(t *testing.T) {
	data, err := os.ReadFile("../../.mega-linter.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Commands []struct {
			Command  string `yaml:"command"`
			CWD      string `yaml:"cwd"`
			Secured  *bool  `yaml:"secured_env"`
			Before   *bool  `yaml:"run_before_linters"`
			Continue *bool  `yaml:"continue_if_failed"`
		} `yaml:"PRE_COMMANDS"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"bash scripts/tests/test-betterleaks-fixture-boundary.sh", "bash scripts/tests/test-secretlint-fixture-boundary.sh"} {
		count := 0
		for _, step := range config.Commands {
			if step.Command != command {
				continue
			}
			if step.CWD != "workspace" || step.Secured == nil || !*step.Secured || step.Before == nil || !*step.Before || step.Continue == nil || *step.Continue {
				t.Fatal("native fixture proof must precede linters, hide credentials and stop on failure")
			}
			count++
		}
		if count != 1 {
			t.Fatalf("require exactly one native fixture refusal proof: %s", command)
		}
	}
}

func TestNativeCNPGSchemaBoundaryIsRequired(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	job := workflow.Jobs["validate-repository-guards"]
	if job.If != "" || job.Continue != nil || len(job.Needs) != 0 {
		t.Fatal("schema provenance and native refusal proof must run unconditionally")
	}
	for _, name := range []string{"deploy-prod", "ci-required-checks"} {
		if !slices.Contains(workflow.Jobs[name].Needs, "validate-repository-guards") {
			t.Fatalf("%s can bypass schema boundary validation", name)
		}
	}
	wanted := "bash scripts/tests/schemas/update-cloudnative-pg-cluster.sh --check-committed\nnpm exec --yes --package=v8r@6.1.0 -- bash scripts/tests/test-v8r-cnpg-schema.sh"
	count := 0
	for _, step := range job.Steps {
		if strings.TrimSpace(step.Run) == wanted {
			if step.If != "" || step.Continue != nil || step.WorkingDirectory != "" || len(step.Env) != 0 {
				t.Fatal("schema proof must use the real root configuration without redirection")
			}
			count++
		}
	}
	if count != 1 {
		t.Fatal("require one pinned native CNPG schema proof and complete schema provenance check")
	}
}

func TestCompletePackageStyleIsAnIndependentRequiredGate(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	job, found := workflow.Jobs["validate-go-package-style"]
	if !found || job.If != "" || job.Continue != nil || len(job.Needs) != 0 || job.Defaults.Run.WorkingDirectory != "" || workflow.Defaults.Run.WorkingDirectory != "" {
		t.Fatal("package style needs an unconditional independent root job")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("package style must be read-only")
	}
	for _, name := range []string{"deploy-prod", "ci-required-checks"} {
		if !slices.Contains(workflow.Jobs[name].Needs, "validate-go-package-style") {
			t.Fatalf("%s can bypass package-style validation", name)
		}
	}
	position := -1
	for _, command := range []string{
		"go install github.com/mgechev/revive@v1.16.0",
		"go test -timeout 1m ./scripts/check-go-package-style",
		"bash scripts/tests/test-revive-package-context.sh",
		"go run ./scripts/check-go-package-style",
	} {
		found := -1
		for index, step := range job.Steps {
			if strings.TrimSpace(step.Run) != command {
				continue
			}
			if found >= 0 || step.If != "" || step.Continue != nil || step.WorkingDirectory != "" || len(step.Env) != 0 {
				t.Fatal("package-style command is repeated, conditional, redirected or suppressed")
			}
			found = index
		}
		if found <= position {
			t.Fatalf("missing or misordered package-style command %s", command)
		}
		position = found
	}
}

func TestPackageStyleResultRefusesMissingOrUnsuccessfulEvidence(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	gate := workflow.Jobs["ci-required-checks"]
	if gate.If != "always()" {
		t.Fatal("required evidence must survive a failed dependency")
	}
	for _, step := range gate.Steps {
		if step.Name != "🧹 Require complete Go package style" {
			continue
		}
		if step.If != "" || step.Continue != nil || step.Env["STYLE_RESULT"] != "${{ needs.validate-go-package-style.result }}" {
			t.Fatal("package-style evidence must be unconditional and bind the actual job")
		}
		for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
			t.Run(result, func(t *testing.T) {
				command := exec.Command("bash", "-e", "-o", "pipefail", "-c", step.Run)
				command.Env = append(os.Environ(), "STYLE_RESULT="+result)
				if err := command.Run(); (err == nil) != (result == "success") {
					t.Fatalf("package-style result %q: %v", result, err)
				}
			})
		}
		return
	}
	t.Fatal("missing required package-style result guard")
}
