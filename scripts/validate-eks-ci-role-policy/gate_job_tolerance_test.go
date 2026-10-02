package main

import (
	"fmt"
	"strings"
	"testing"
)

// gateJobToleranceFindings reports, in a stable order, every job deciding
// whether a gate runs — the gate jobs and the jobs they need, as
// gateDecidingJobs walks them — that tolerates its own failure.
//
// A job-level `continue-on-error` keeps the run green when the job fails, so on
// a gate job every job that needs it carries on after the gate rejects the
// change, and upstream of one the gate runs on, or is skipped by, the outputs of
// a job that failed. The coverage guards read the gate as present in both
// cases, which is the fail-open #4361 names. An expression-valued
// `continue-on-error` counts too: its value is unknowable here, and the
// reviewed-literal escape the job-condition table offers has no use while no
// gate-deciding job needs one, so none is offered. Unreadable `needs:` are left
// to the job-condition test, which already reports them.
func gateJobToleranceFindings(workflows map[string]workflow) []string {
	var findings []string
	for _, name := range sortedKeys(workflows) {
		deciding, _ := gateDecidingJobs(name, workflows[name])
		for _, jobName := range sortedKeys(deciding) {
			tolerance := workflows[name].Jobs[jobName].ContinueOnError
			if errorIsTolerated(tolerance) {
				findings = append(findings, fmt.Sprintf(
					"%s/%s decides whether a gate runs but tolerates its own failure (continue-on-error: %v)",
					name, jobName, tolerance))
			}
		}
	}
	return findings
}

// TestGateJobsDoNotTolerateFailure fails when a job that decides whether a gate
// runs carries a truthy or expression-valued `continue-on-error`.
func TestGateJobsDoNotTolerateFailure(t *testing.T) {
	for _, finding := range gateJobToleranceFindings(loadWorkflows(t)) {
		t.Errorf("%s. The run stays green when that job fails, so the gate decides nothing; "+
			"remove continue-on-error from the job.", finding)
	}
}

// TestGateJobToleranceGuardIsNotVacuous perturbs the real workflows, asserts
// each perturbation was applied, and checks the guard names the job — and that
// it does not flag the default spelled out.
func TestGateJobToleranceGuardIsNotVacuous(t *testing.T) {
	if findings := gateJobToleranceFindings(loadWorkflows(t)); len(findings) != 0 {
		t.Fatalf("positive control failed: no real gate-deciding job may tolerate failure, got %v", findings)
	}

	tolerated := func(t *testing.T, key string, value any) map[string]workflow {
		t.Helper()
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(key, "/")
		parsedJob, ok := workflows[name].Jobs[jobName]
		if !ok {
			t.Fatalf("%s is missing, so this control cannot be applied", key)
		}
		if errorIsTolerated(parsedJob.ContinueOnError) == errorIsTolerated(value) {
			t.Fatalf("the perturbation of %s to %v would not change whether it tolerates failure", key, value)
		}
		parsedJob.ContinueOnError = value
		workflows[name].Jobs[jobName] = parsedJob
		return workflows
	}

	expectFinding := func(t *testing.T, findings []string, key string) {
		t.Helper()
		for _, finding := range findings {
			if strings.HasPrefix(finding, key+" ") {
				return
			}
		}
		t.Fatalf("want a finding for %s, got %v", key, findings)
	}

	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"a tolerated authorization gate job", "ci.yaml/validate-eks-authorization", true},
		{"an expression-valued tolerance", "cd.yaml/validate-eks-authorization", "${{ inputs.skip-gate }}"},
		{"a tolerated isolated-chart gate job", "dr-rebuild.yaml/rebuild", true},
		{"a tolerated job a gate job needs", "ci.yaml/changes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectFinding(t, gateJobToleranceFindings(tolerated(t, tc.key, tc.value)), tc.key)
		})
	}

	t.Run("an explicit continue-on-error: false is not a finding", func(t *testing.T) {
		workflows := loadWorkflows(t)
		parsedJob := workflows["ci.yaml"].Jobs["validate-eks-authorization"]
		parsedJob.ContinueOnError = false
		workflows["ci.yaml"].Jobs["validate-eks-authorization"] = parsedJob
		if workflows["ci.yaml"].Jobs["validate-eks-authorization"].ContinueOnError != false {
			t.Fatal("the perturbation was not applied")
		}
		if findings := gateJobToleranceFindings(workflows); len(findings) != 0 {
			t.Fatalf("`continue-on-error: false` is the default spelled out, got %v", findings)
		}
	})
}
