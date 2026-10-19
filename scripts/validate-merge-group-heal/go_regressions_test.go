package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type regressionRunDefaults struct {
	Run struct {
		WorkingDirectory string `yaml:"working-directory"`
	} `yaml:"run"`
}

type regressionWorkflow struct {
	Defaults regressionRunDefaults `yaml:"defaults"`
	Jobs     map[string]struct {
		Defaults    regressionRunDefaults `yaml:"defaults"`
		If          string                `yaml:"if"`
		Needs       []string              `yaml:"needs"`
		Continue    any                   `yaml:"continue-on-error"`
		Permissions map[string]string     `yaml:"permissions"`
		Steps       []struct {
			Name             string            `yaml:"name"`
			If               string            `yaml:"if"`
			Run              string            `yaml:"run"`
			Continue         any               `yaml:"continue-on-error"`
			Env              map[string]string `yaml:"env"`
			WorkingDirectory string            `yaml:"working-directory"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// readRegressionWorkflow loads the actual CI workflow rather than a separate fixture.
func readRegressionWorkflow(t *testing.T) regressionWorkflow {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow regressionWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

// Full-suite work is independent of path classification. Serializing it ahead
// of changes delays every selected validator without adding a safety barrier.
func TestFullGoRegressionsRunIndependently(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	job, found := workflow.Jobs["validate-go-regressions"]
	if !found {
		t.Fatal("full Go regressions need an independent validation job")
	}
	if len(job.Needs) != 0 || job.If != "" || job.Continue != nil {
		t.Fatal("full regressions must start independently and run unconditionally")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("the regression runner needs only read-only checkout permission")
	}
	for _, command := range []string{
		"go test ./...",
		"go -C scripts/retire-backstage-stateless/poller-native test -race -mod=readonly -count=1 ./...",
	} {
		found := false
		for name, candidate := range workflow.Jobs {
			for _, step := range candidate.Steps {
				if strings.TrimSpace(step.Run) != command {
					continue
				}
				if name != "validate-go-regressions" || step.If != "" || step.Continue != nil {
					t.Fatalf("complete suite must run once, unconditionally in its independent job: %s", command)
				}
				for _, directory := range []string{
					workflow.Defaults.Run.WorkingDirectory,
					candidate.Defaults.Run.WorkingDirectory,
					step.WorkingDirectory,
				} {
					if directory != "" && directory != "." && directory != "./" {
						t.Fatalf("complete suite must run from repository root, not %q: %s", directory, command)
					}
				}
				if found {
					t.Fatalf("duplicate complete-suite invocation: %s", command)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("complete suite was removed or narrowed: %s", command)
		}
	}
	for _, name := range []string{"deploy-prod", "ci-required-checks"} {
		if !slices.Contains(workflow.Jobs[name].Needs, "validate-go-regressions") {
			t.Fatalf("%s must wait for full Go regressions", name)
		}
	}
	deploy := workflow.Jobs["deploy-prod"]
	if deploy.Continue != nil || strings.Contains(deploy.If, "always(") || strings.Contains(deploy.If, "cancelled(") || strings.Contains(deploy.If, "failure(") {
		t.Fatal("deployment must not bypass failed or skipped dependencies")
	}
}

// Execute the actual aggregate guard: GitHub considers some skipped checks
// successful, but an unconditional full suite has no legitimate skipped result.
func TestFullGoRegressionsRejectEveryNonSuccessResult(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	gate := workflow.Jobs["ci-required-checks"]
	if gate.If != "always()" {
		t.Fatal("the aggregate must still run after a failed dependency")
	}
	for _, step := range gate.Steps {
		if step.Name != "🧪 Require complete Go regressions" {
			continue
		}
		if step.If != "" || step.Continue != nil || step.Env["REGRESSION_RESULT"] != "${{ needs.validate-go-regressions.result }}" {
			t.Fatal("full-suite result enforcement must be unconditional and bind the real job")
		}
		for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
			t.Run(result, func(t *testing.T) {
				command := exec.Command("bash", "-e", "-o", "pipefail", "-c", step.Run)
				command.Env = append(os.Environ(), "REGRESSION_RESULT="+result)
				output, err := command.CombinedOutput()
				if (err == nil) != (result == "success") {
					t.Fatalf("result %q: error=%v, output=%s", result, err, output)
				}
			})
		}
		return
	}
	t.Fatal("required aggregate has no full-suite result enforcement")
}
