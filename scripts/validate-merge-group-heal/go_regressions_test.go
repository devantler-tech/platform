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
		TimeoutMinutes int                   `yaml:"timeout-minutes"`
		Defaults       regressionRunDefaults `yaml:"defaults"`
		If             string                `yaml:"if"`
		Needs          []string              `yaml:"needs"`
		Continue       any                   `yaml:"continue-on-error"`
		Permissions    map[string]string     `yaml:"permissions"`
		Strategy       struct {
			FailFast    *bool               `yaml:"fail-fast"`
			MaxParallel int                 `yaml:"max-parallel"`
			Matrix      map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			TimeoutMinutes   int               `yaml:"timeout-minutes"`
			Name             string            `yaml:"name"`
			Uses             string            `yaml:"uses"`
			With             map[string]any    `yaml:"with"`
			If               string            `yaml:"if"`
			Run              string            `yaml:"run"`
			Continue         any               `yaml:"continue-on-error"`
			Env              map[string]string `yaml:"env"`
			WorkingDirectory string            `yaml:"working-directory"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestGoLintIsAnUnconditionalReadOnlyGate(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	job, found := workflow.Jobs["validate-go-lint"]
	if !found || len(job.Needs) != 0 || job.If != "" || job.Continue != nil || job.TimeoutMinutes != 15 {
		t.Fatal("complete lint needs its own unconditional independent job within 15 minutes")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("the lint runner needs only read-only checkout permission")
	}
	if !completeLintPartitions(workflow) {
		t.Fatal("complete lint requires both fixed partitions, two workers and no early cancellation")
	}
	for _, name := range []string{"deploy-prod", "ci-required-checks"} {
		if !slices.Contains(workflow.Jobs[name].Needs, "validate-go-lint") {
			t.Fatalf("%s must wait for complete Go lint", name)
		}
	}
	install, control, lint := -1, -1, -1
	for index, step := range job.Steps {
		switch {
		case strings.HasPrefix(step.Uses, "golangci/golangci-lint-action@"):
			if install >= 0 || len(strings.TrimPrefix(step.Uses, "golangci/golangci-lint-action@")) != 40 || step.With["version"] != "v2.14.0" || step.With["install-only"] != true || step.If != "" || step.Continue != nil {
				t.Fatal("lint must install one pinned, catalogue-aligned binary unconditionally")
			}
			install = index
		case strings.TrimSpace(step.Run) == "bash scripts/tests/test-go-modules-lint.sh":
			if control >= 0 || step.If != "" || step.Continue != nil || step.WorkingDirectory != "" || step.TimeoutMinutes != 5 {
				t.Fatal("real linter negative controls must run unconditionally from the root")
			}
			control = index
		case strings.TrimSpace(step.Run) == `bash scripts/check-go-modules-lint.sh "$LINT_PARTITION"`:
			if lint >= 0 || step.If != "" || step.Continue != nil || step.WorkingDirectory != "" || step.TimeoutMinutes != 10 || step.Env["LINT_PARTITION"] != "${{ matrix.partition }}" {
				t.Fatal("complete module lint must run unconditionally from the root")
			}
			lint = index
		}
	}
	if install < 0 || control <= install || lint <= control {
		t.Fatal("independent lint must install the linter, verify its real controls, then lint the tracked modules")
	}
}

func completeLintPartitions(workflow regressionWorkflow) bool {
	strategy := workflow.Jobs["validate-go-lint"].Strategy
	parts := append([]string(nil), strategy.Matrix["partition"]...)
	slices.Sort(parts)
	return strategy.FailFast != nil && !*strategy.FailFast && strategy.MaxParallel == 2 && len(strategy.Matrix) == 1 &&
		slices.Equal(parts, []string{"part-0", "part-1"})
}

func TestGoLintPartitionsRejectIncompleteRouting(t *testing.T) {
	t.Run("extra axis", func(t *testing.T) {
		workflow := readRegressionWorkflow(t)
		job := workflow.Jobs["validate-go-lint"]
		job.Strategy.Matrix["extra"] = []string{"ignored"}
		workflow.Jobs["validate-go-lint"] = job
		if completeLintPartitions(workflow) {
			t.Fatal("accepted an extra lint routing axis")
		}
	})
	for _, test := range []struct {
		name     string
		parts    []string
		workers  int
		failFast *bool
	}{
		{"missing", []string{"part-0"}, 2, new(false)},
		{"duplicate", []string{"part-0", "part-0"}, 2, new(false)},
		{"unknown", []string{"part-0", "other"}, 2, new(false)},
		{"extra", []string{"part-0", "part-1", "all"}, 2, new(false)},
		{"unbounded", []string{"part-0", "part-1"}, 0, new(false)},
		{"overflow", []string{"part-0", "part-1"}, 3, new(false)},
		{"early cancellation", []string{"part-0", "part-1"}, 2, new(true)},
		{"default cancellation", []string{"part-0", "part-1"}, 2, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := readRegressionWorkflow(t)
			job := workflow.Jobs["validate-go-lint"]
			job.Strategy.Matrix = map[string][]string{"partition": test.parts}
			job.Strategy.MaxParallel, job.Strategy.FailFast = test.workers, test.failFast
			workflow.Jobs["validate-go-lint"] = job
			if completeLintPartitions(workflow) {
				t.Fatal("accepted incomplete or unbounded lint routing")
			}
		})
	}
}

func TestGoLintRejectsEveryNonSuccessResult(t *testing.T) {
	workflow := readRegressionWorkflow(t)
	gate := workflow.Jobs["ci-required-checks"]
	if gate.If != "always()" {
		t.Fatal("the aggregate must still run after a failed dependency")
	}
	for _, step := range gate.Steps {
		if step.Name != "🧹 Require complete Go lint" {
			continue
		}
		if step.If != "" || step.Continue != nil || step.Env["LINT_RESULT"] != "${{ needs.validate-go-lint.result }}" {
			t.Fatal("complete lint enforcement must be unconditional and bind the real job")
		}
		for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
			t.Run(result, func(t *testing.T) {
				command := exec.Command("bash", "-e", "-o", "pipefail", "-c", step.Run)
				command.Env = append(os.Environ(), "LINT_RESULT="+result)
				output, err := command.CombinedOutput()
				if (err == nil) != (result == "success") {
					t.Fatalf("result %q: error=%v, output=%s", result, err, output)
				}
			})
		}
		return
	}
	t.Fatal("required aggregate has no complete-lint result enforcement")
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
	// Keep the bounded test concurrency and explicit per-package timeout in
	// the pinned command without allowing package filters or skipped results.
	positions := make(map[string]int)
	for _, command := range []string{
		"bash scripts/tests/test-go-modules-tidy.sh",
		"bash scripts/check-go-modules-tidy.sh",
		"go test -parallel 8 -timeout 20m ./...",
	} {
		found := false
		for name, candidate := range workflow.Jobs {
			for index, step := range candidate.Steps {
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
				positions[command] = index
			}
		}
		if !found {
			t.Fatalf("complete suite was removed or narrowed: %s", command)
		}
	}
	if positions["bash scripts/tests/test-go-modules-tidy.sh"] >= positions["bash scripts/check-go-modules-tidy.sh"] ||
		positions["bash scripts/check-go-modules-tidy.sh"] >= positions["go test -parallel 8 -timeout 20m ./..."] {
		t.Fatal("module refusal controls and all-module guard must pass before the full Go suite")
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
