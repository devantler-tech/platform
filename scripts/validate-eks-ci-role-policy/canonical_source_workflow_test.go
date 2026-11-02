package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var canonicalCICommands = map[string]string{
	"bash scripts/run-publication-generator-tests.sh --jobs 4":   "validate-publication-generator",
	"bash scripts/tests/test-run-publication-generator-tests.sh": "validate-publication-generator",
	"bash scripts/tests/test-tenant-source-discovery.sh":         "validate-independent-guards",
	"./scripts/guard-consumer-discovery-conservation.sh":         "validate-independent-guards",
}

var canonicalCIPaths = []string{
	"k8s/**", "ksail.prod.yaml", "talos*/**", ".github/workflows/ci.yaml",
	"scripts/validate-eks-ci-role-policy/**",
	"scripts/canonical-tenant-source.lib.sh",
	"scripts/canonical-tenant-source-bindings.json",
	"scripts/tenant-source-discovery.lib.sh",
	"scripts/tenant-publication-binding.lib.sh",
	"scripts/bootstrap-tenant-source.sh",
	"scripts/tests/lib/legacy-consumer-fixture.sh",
	"scripts/publish-workflow-canonical-approvals.tsv",
	"scripts/publish-workflow-approved-revisions.tsv",
	"scripts/publish-workflow-approved-revisions.identities.tsv",
	"scripts/publish-workflow-bootstrap-publications.json",
	"scripts/shared-publish-workflow-matchers.tsv",
	"scripts/generate-publish-workflow-approved-revisions.sh",
	"scripts/run-publication-generator-tests.sh",
	"scripts/tests/test-run-publication-generator-tests.sh",
	"scripts/tests/test-canonical-source-bootstrap.sh",
	"scripts/tests/test-canonical-source-upgrade.sh",
	"scripts/tests/test-canonical-constructor-attribution.sh",
	"scripts/tests/test-canonical-generator-output.sh",
	"scripts/tests/test-canonical-bootstrap-output.sh",
	"scripts/tests/test-canonical-publisher-regeneration.sh",
	"scripts/tests/test-tenant-source-discovery.sh",
	"scripts/tests/test-tenant-kro-pull-secret.sh",
	"scripts/tests/tenant-webapp-admission/**",
}

// This package runs independently through the unconditional Go regressions.
// Removing a canonical shell step must not also remove its registration check.
func TestCanonicalSourceCIRegistration(t *testing.T) {
	if err := canonicalSourceCIContract(canonicalCIWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

// The workflow now delegates scheduling to the real bounded runner. Exercise
// that boundary independently of its shell self-test: dropping a fixture or
// masking any child failure must still fail the unconditional Go regressions.
func TestCanonicalPublicationRunnerKeepsEveryFixtureAndFailure(t *testing.T) {
	source, err := os.ReadFile("../run-publication-generator-tests.sh")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []string{
		"test-publish-workflow-approved-revisions.sh",
		"test-tenant-publication-binding.sh",
		"test-bootstrap-tenant-source.sh",
		"test-canonical-source-bootstrap.sh",
		"test-canonical-source-upgrade.sh",
		"test-canonical-constructor-attribution.sh",
		"test-canonical-generator-output.sh",
		"test-canonical-bootstrap-output.sh",
		"test-canonical-publisher-regeneration.sh",
	}
	root := t.TempDir()
	tests := filepath.Join(root, "scripts", "tests")
	if err := os.MkdirAll(tests, 0700); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(root, "scripts", "run-publication-generator-tests.sh")
	if err := os.WriteFile(runner, source, 0600); err != nil {
		t.Fatal(err)
	}
	for failure := -1; failure < len(fixtures); failure++ {
		name := "all-pass"
		if failure >= 0 {
			name = fixtures[failure]
		}
		t.Run(name, func(t *testing.T) {
			for index, fixture := range fixtures {
				status := 0
				if index == failure {
					status = 255
				}
				body := fmt.Sprintf("#!/usr/bin/env bash\nprintf 'fixture %s\\n'\nexit %d\n", fixture, status)
				if err := os.WriteFile(filepath.Join(tests, fixture), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "bash", runner, "--jobs", "4").CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("runner exceeded the bounded fixture deadline: %v\n%s", ctx.Err(), output)
			}
			if failure == -1 {
				if err != nil {
					t.Fatalf("complete successful fixtures failed: %v\n%s", err, output)
				}
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				t.Fatalf("child failure must fail the runner, got %v\n%s", err, output)
			}
			for _, fixture := range fixtures {
				if strings.Count(string(output), "fixture "+fixture+"\n") != 1 {
					t.Fatalf("fixture %s was omitted or repeated, including after failure\n%s", fixture, output)
				}
			}
		})
	}
}

// canonicalCIWorkflow loads the actual workflow so every mutation starts from shipping source.
func canonicalCIWorkflow(t *testing.T) workflow {
	t.Helper()
	body, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

// mutateCanonicalCommand changes exactly one registered command in its owning CI job.
func mutateCanonicalCommand(t *testing.T, wf *workflow, command string, mutate func(*step)) {
	t.Helper()
	owner := canonicalCICommands[command]
	j := wf.Jobs[owner]
	found := 0
	for i := range j.Steps {
		if strings.Contains(j.Steps[i].Run, command) {
			found++
			mutate(&j.Steps[i])
		}
	}
	if found != 1 {
		t.Fatalf("mutation requires exactly one native command block, got %d for %s", found, command)
	}
	wf.Jobs[owner] = j
}

// mutateCanonicalFilter applies a control to the real source-selection configuration.
func mutateCanonicalFilter(t *testing.T, wf *workflow, mutate func(map[string][]string)) {
	t.Helper()
	j := wf.Jobs["changes"]
	found := 0
	for i := range j.Steps {
		if j.Steps[i].ID != "filter" {
			continue
		}
		found++
		var filters map[string][]string
		if err := yaml.Unmarshal([]byte(j.Steps[i].With.Filters), &filters); err != nil {
			t.Fatal(err)
		}
		mutate(filters)
		body, err := yaml.Marshal(filters)
		if err != nil {
			t.Fatal(err)
		}
		j.Steps[i].With.Filters = string(body)
	}
	if found != 1 {
		t.Fatalf("mutation requires one native filter step, got %d", found)
	}
	wf.Jobs["changes"] = j
}

// TestCanonicalSourceCIRejectsLostOrNeutralizedCommands rejects absent or masked source regressions.
func TestCanonicalSourceCIRejectsLostOrNeutralizedCommands(t *testing.T) {
	for command := range canonicalCICommands {
		for name, replacement := range map[string]string{
			"removed": "", "comment": "# " + command, "echo": "echo " + command,
			"conditional": "if false; then " + command + "; fi",
			"masked":      command + " || true", "and-list": command + " && true",
		} {
			t.Run(command+"/"+name, func(t *testing.T) {
				wf := canonicalCIWorkflow(t)
				mutateCanonicalCommand(t, &wf, command, func(s *step) {
					if strings.Count(s.Run, command) != 1 {
						t.Fatal("mutation is not unique")
					}
					s.Run = strings.Replace(s.Run, command, replacement, 1)
				})
				if canonicalSourceCIContract(wf) == nil {
					t.Fatal("accepted a missing or neutralized real CI command")
				}
			})
		}
		for name, mutate := range map[string]func(*step){
			"skipped":         func(s *step) { s.If = "false" },
			"tolerated":       func(s *step) { s.ContinueOnError = true },
			"expression":      func(s *step) { s.ContinueOnError = "${{ true }}" },
			"non-errexit":     func(s *step) { s.Shell = "bash {0}" },
			"early-exit":      func(s *step) { s.Run = "exit 0\n" + s.Run },
			"disable-errexit": func(s *step) { s.Run = "set +e\n" + s.Run },
			"duplicate":       func(s *step) { s.Run += "\n" + command },
		} {
			t.Run(command+"/"+name, func(t *testing.T) {
				wf := canonicalCIWorkflow(t)
				mutateCanonicalCommand(t, &wf, command, mutate)
				if canonicalSourceCIContract(wf) == nil {
					t.Fatal("accepted a bypassed or duplicated real CI command")
				}
			})
		}
	}
}

// TestCanonicalSourceCIRejectsLostSourceSelection protects every registered source trigger.
func TestCanonicalSourceCIRejectsLostSourceSelection(t *testing.T) {
	for _, required := range canonicalCIPaths {
		for _, move := range []bool{false, true} {
			name := "removed/"
			if move {
				name = "wrong-bucket/"
			}
			t.Run(name+required, func(t *testing.T) {
				wf := canonicalCIWorkflow(t)
				mutateCanonicalFilter(t, &wf, func(filters map[string][]string) {
					var kept []string
					removed := 0
					for _, item := range filters["k8s"] {
						if item == required {
							removed++
						} else {
							kept = append(kept, item)
						}
					}
					if removed == 0 {
						t.Fatal("mutation did not remove a real selection")
					}
					filters["k8s"] = kept
					if move {
						filters["another-bucket"] = append(filters["another-bucket"], required)
					}
				})
				if canonicalSourceCIContract(wf) == nil {
					t.Fatal("accepted lost native source selection")
				}
			})
		}
	}
}

// TestCanonicalSourceCIRejectsGateAndFilterBypasses checks scheduling, dependency and shell boundaries.
func TestCanonicalSourceCIRejectsGateAndFilterBypasses(t *testing.T) {
	for name, mutate := range map[string]func(*workflow){
		"missing guard job": func(w *workflow) { delete(w.Jobs, "validate-repository-guards") },
		"conditional guard job": func(w *workflow) {
			j := w.Jobs["validate-repository-guards"]
			j.If = "false"
			w.Jobs["validate-repository-guards"] = j
		},
		"tolerated guard job": func(w *workflow) {
			j := w.Jobs["validate-repository-guards"]
			j.ContinueOnError = "${{ true }}"
			w.Jobs["validate-repository-guards"] = j
		},
		"workflow shell override": func(w *workflow) {
			w.Defaults.Run.Shell = "bash {0}"
			j := w.Jobs["validate-independent-guards"]
			j.Defaults.Run.Shell = ""
			for i := range j.Steps {
				j.Steps[i].Shell = ""
			}
			w.Jobs["validate-independent-guards"] = j
		},
		"job shell override": func(w *workflow) {
			j := w.Jobs["validate-independent-guards"]
			j.Defaults.Run.Shell = "bash {0}"
			for i := range j.Steps {
				j.Steps[i].Shell = ""
			}
			w.Jobs["validate-independent-guards"] = j
		},
		"missing changes job": func(w *workflow) { delete(w.Jobs, "changes") },
		"conditional changes": func(w *workflow) {
			j := w.Jobs["changes"]
			j.If = "false"
			w.Jobs["changes"] = j
		},
		"tolerated changes": func(w *workflow) {
			j := w.Jobs["changes"]
			j.ContinueOnError = true
			w.Jobs["changes"] = j
		},
		"missing filter": func(w *workflow) {
			j := w.Jobs["changes"]
			for i := range j.Steps {
				if j.Steps[i].ID == "filter" {
					j.Steps[i].ID = "lost-filter"
				}
			}
			w.Jobs["changes"] = j
		},
		"conditional filter": func(w *workflow) {
			j := w.Jobs["changes"]
			for i := range j.Steps {
				if j.Steps[i].ID == "filter" {
					j.Steps[i].If = "false"
				}
			}
			w.Jobs["changes"] = j
		},
		"tolerated filter": func(w *workflow) {
			j := w.Jobs["changes"]
			for i := range j.Steps {
				if j.Steps[i].ID == "filter" {
					j.Steps[i].ContinueOnError = "${{ true }}"
				}
			}
			w.Jobs["changes"] = j
		},
	} {
		t.Run(name, func(t *testing.T) {
			wf := canonicalCIWorkflow(t)
			mutate(&wf)
			if canonicalSourceCIContract(wf) == nil {
				t.Fatal("accepted a bypassed native gate")
			}
		})
	}
	wf := canonicalCIWorkflow(t)
	j := wf.Jobs["validate-repository-guards"]
	j.ContinueOnError = false
	for i := range j.Steps {
		j.Steps[i].ContinueOnError = false
	}
	wf.Jobs["validate-repository-guards"] = j
	j = wf.Jobs["changes"]
	j.ContinueOnError = false
	for i := range j.Steps {
		j.Steps[i].ContinueOnError = false
	}
	wf.Jobs["changes"] = j
	if err := canonicalSourceCIContract(wf); err != nil {
		t.Fatalf("rejected explicit false: %v", err)
	}
	for _, scope := range []string{"job", "step"} {
		wf = canonicalCIWorkflow(t)
		wf.Defaults.Run.Shell = "bash {0}"
		for _, name := range []string{"validate-repository-guards", "validate-independent-guards", "validate-publication-generator"} {
			j = wf.Jobs[name]
			j.Defaults.Run.Shell = "bash"
			if scope == "step" {
				j.Defaults.Run.Shell = "bash {0}"
			}
			for i := range j.Steps {
				if scope == "step" {
					j.Steps[i].Shell = "bash"
				} else {
					j.Steps[i].Shell = ""
				}
			}
			wf.Jobs[name] = j
		}
		if err := canonicalSourceCIContract(wf); err != nil {
			t.Fatalf("rejected safe %s shell override: %v", scope, err)
		}
	}
}

// TestCanonicalGeneratorCIRejectsLostRequiredJob requires generator success before deployment and CI.
func TestCanonicalGeneratorCIRejectsLostRequiredJob(t *testing.T) {
	for _, mutation := range []string{"missing", "conditional", "tolerated", "unsafe-shell", "deploy-prod", "ci-required-checks"} {
		t.Run(mutation, func(t *testing.T) {
			wf := canonicalCIWorkflow(t)
			if err := canonicalSourceCIContract(wf); err != nil {
				t.Fatalf("positive control: %v", err)
			}
			name := "validate-publication-generator"
			j := wf.Jobs[name]
			want := "require unconditional " + name
			switch mutation {
			case "missing":
				delete(wf.Jobs, name)
			case "conditional":
				j.If = "false"
				wf.Jobs[name] = j
			case "tolerated":
				j.ContinueOnError = true
				wf.Jobs[name] = j
			case "unsafe-shell":
				j.Defaults.Run.Shell = "bash {0}"
				wf.Jobs[name] = j
				want = "canonical source CI must enforce exactly one"
			default:
				consumer := wf.Jobs[mutation]
				needs, valid := consumer.needsOf()
				if !valid || !slices.Contains(needs, name) {
					t.Fatal("mutation must remove an actual required dependency")
				}
				kept := make([]any, 0, len(needs)-1)
				for _, dependency := range needs {
					if dependency != name {
						kept = append(kept, dependency)
					}
				}
				consumer.Needs = kept
				wf.Jobs[mutation] = consumer
				want = mutation + " must require the publication generator"
			}
			err := canonicalSourceCIContract(wf)
			if err == nil {
				t.Fatal("accepted lost generator enforcement")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong refusal: want %q in %v", want, err)
			}
		})
	}
}

// canonicalSourceCIContract verifies source coverage and the required path to production authorization.
func canonicalSourceCIContract(wf workflow) error {
	for _, name := range []string{"validate-repository-guards", "validate-independent-guards", "validate-publication-generator"} {
		guard, ok := wf.Jobs[name]
		if !ok || guard.If != "" || errorIsTolerated(guard.ContinueOnError) {
			return fmt.Errorf("canonical source contracts require unconditional %s", name)
		}
	}
	for _, consumer := range []string{"deploy-prod", "ci-required-checks"} {
		needs, valid := wf.Jobs[consumer].needsOf()
		if !valid || !slices.Contains(needs, "validate-publication-generator") {
			return fmt.Errorf("%s must require the publication generator", consumer)
		}
		if !slices.Contains(needs, "validate-independent-guards") {
			return fmt.Errorf("%s must require the independent guards", consumer)
		}
	}
	for command, owner := range canonicalCICommands {
		guard := wf.Jobs[owner]
		count := 0
		for _, s := range guard.Steps {
			shell := s.Shell
			if shell == "" {
				shell = guard.Defaults.Run.Shell
			}
			if shell == "" {
				shell = wf.Defaults.Run.Shell
			}
			if s.Uses == "" && s.enforces() && shellKeepsErrexit(shell) {
				count += canonicalCommandCount(s.Run, command)
			}
		}
		if count != 1 {
			return fmt.Errorf("canonical source CI must enforce exactly one %s invocation, got %d", command, count)
		}
	}
	changes, ok := wf.Jobs["changes"]
	if !ok || changes.If != "" || errorIsTolerated(changes.ContinueOnError) {
		return fmt.Errorf("canonical source selection requires the unconditional changes job")
	}
	var filters map[string][]string
	found := 0
	for _, s := range changes.Steps {
		if s.ID != "filter" {
			continue
		}
		found++
		if !s.enforces() || !strings.HasPrefix(s.Uses, "dorny/paths-filter@") || s.With.Filters == "" {
			return fmt.Errorf("canonical source selection requires an enforcing native paths filter")
		}
		if err := yaml.Unmarshal([]byte(s.With.Filters), &filters); err != nil {
			return fmt.Errorf("parse canonical source selection: %w", err)
		}
	}
	if found != 1 {
		return fmt.Errorf("canonical source selection requires exactly one native filter")
	}
	selected := make(map[string]bool)
	for _, pattern := range filters["k8s"] {
		selected[pattern] = true
	}
	for _, path := range canonicalCIPaths {
		if !selected[path] {
			return fmt.Errorf("k8s selection omits canonical input %s", path)
		}
	}
	return approvedSignerRoute(wf, "validate-independent-guards", "deploy-prod", "")
}

// Every non-comment line in this fixed CI block must remain a simple command.
// This reuses the existing positive shell grammar, so an earlier exit, disabled
// errexit or conditional cannot make a later textual mention count as coverage.
func canonicalCommandCount(script, command string) int {
	count := 0
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !isSimpleCommandLine(line) {
			return 0
		}
		if strings.Join(strings.Fields(line), " ") == command {
			count++
		}
	}
	return count
}
