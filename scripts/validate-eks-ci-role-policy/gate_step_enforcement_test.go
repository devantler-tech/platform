package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// withGateStep loads a fresh copy of the real workflows and rewrites the step in
// the job `<workflow>/<job>` that runs invocation. Jobs are map values and steps
// a slice inside them, so the edit is written back explicitly — and asserted
// applied, so a control cannot pass on an unchanged tree.
func withGateStep(t *testing.T, key, invocation string, edit func(*step)) map[string]workflow {
	t.Helper()
	workflows := loadWorkflows(t)
	name, jobName, _ := strings.Cut(key, "/")
	parsedJob, ok := workflows[name].Jobs[jobName]
	if !ok {
		t.Fatalf("%s is missing, so this control cannot be applied", key)
	}
	index := slices.IndexFunc(parsedJob.Steps, func(s step) bool { return runsGate(s.Run, invocation) })
	if index < 0 {
		t.Fatalf("%s runs no step invoking %q, so this control cannot be applied", key, invocation)
	}
	steps := slices.Clone(parsedJob.Steps)
	edit(&steps[index])
	if reflect.DeepEqual(steps[index], parsedJob.Steps[index]) {
		t.Fatalf("the perturbation of the %q step in %s was not applied", invocation, key)
	}
	parsedJob.Steps = steps
	workflows[name].Jobs[jobName] = parsedJob
	return workflows
}

// gateStepDisarms are the step-level edits that leave a gate written into a
// route while taking away its power to stop the job (#4361).
var gateStepDisarms = map[string]func(*step){
	"a step condition":             func(s *step) { s.If = "false" },
	"a tolerated failure":          func(s *step) { s.ContinueOnError = true },
	"an expression-valued failure": func(s *step) { s.ContinueOnError = "${{ github.event_name == 'merge_group' }}" },
}

// TestGateStepEnforcementGuardIsNotVacuous applies each disarm to a real gate
// step, asserts it was applied, and checks the route then reads as uncovered.
//
// The authorization gate is judged here on the push-to-main route, the one
// whose step this grammar reads. In ci.yaml and cd.yaml the same gate runs
// inside a compound script (both commands run, and the step fails if either
// did, #3879), which the line grammar deliberately refuses; that step's
// condition and tolerance are pinned by
// scripts/tests/test-authorization-gate-diagnostics.sh, which already extracts
// and executes it.
func TestGateStepEnforcementGuardIsNotVacuous(t *testing.T) {
	workflows := loadWorkflows(t)
	const pushRoute = "validate-main.yaml"
	if !workflows[pushRoute].coversPushToMain() {
		t.Fatalf("positive control failed: %s must run the authorization gate on push to main", pushRoute)
	}
	for _, label := range sortedKeys(gateStepDisarms) {
		t.Run(label+" on the authorization gate", func(t *testing.T) {
			perturbed := withGateStep(t, pushRoute+"/validate-eks-authorization", validatorInvocation,
				gateStepDisarms[label])
			if perturbed[pushRoute].coversPushToMain() {
				t.Fatalf("%s on the authorization step must leave push to main uncovered", label)
			}
		})
	}

	// Routes where ONE job runs the isolated-chart gate, so disarming its step
	// must flip the workflow. ci.yaml runs it in two jobs, so the workflow-level
	// question stays true there by design.
	for _, key := range []string{
		"cd.yaml/validate-eks-authorization",
		"dr-rebuild.yaml/rebuild",
		"validate-main.yaml/validate-eks-authorization",
	} {
		name, _, _ := strings.Cut(key, "/")
		if !workflows[name].runsIsolatedChartNamespaceValidator() {
			t.Fatalf("positive control failed: %s must run the isolated-chart gate", name)
		}
		for _, label := range sortedKeys(gateStepDisarms) {
			t.Run(key+": "+label+" on the isolated-chart gate", func(t *testing.T) {
				perturbed := withGateStep(t, key, isolatedChartNamespaceValidatorInvocation, gateStepDisarms[label])
				if perturbed[name].runsIsolatedChartNamespaceValidator() {
					t.Fatalf("%s on the isolated-chart step must leave %s uncovered", label, name)
				}
			})
		}
	}

	t.Run("an explicit continue-on-error: false still counts", func(t *testing.T) {
		perturbed := withGateStep(t, pushRoute+"/validate-eks-authorization", validatorInvocation,
			func(s *step) { s.ContinueOnError = false })
		if !perturbed[pushRoute].coversPushToMain() {
			t.Fatal("`continue-on-error: false` is the default spelled out and must not read as a disarm")
		}
	})
}
