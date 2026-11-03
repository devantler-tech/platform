package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

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
