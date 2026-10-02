package main

import (
	"fmt"
	"strings"
	"testing"
)

// reviewedGateJobConditions pins the `if:` of every job that runs one of the
// gates the coverage guards in this package look for, keyed
// `<workflow>/<job>`.
//
// Those guards ask whether a gate is written into a route's job. They cannot
// ask whether the job runs: a job-level condition is evaluated by Actions at
// run time, so a gate in a job that is skipped on some runs still reads as
// covered on every run. Rejecting conditional jobs outright would fail the
// deliberate ones — the supersession gate on recovery, the event filter on CI,
// the failure filter on the heal path — and evaluating a condition is not
// possible here. So each condition is pinned to the literal that was reviewed:
// any edit to it, removing it, or adding one to an unconditional gate job fails
// this test until the entry below is updated in the same change, which puts
// the new condition in front of a reviewer.
//
// An empty string means the job is reviewed as unconditional. Whitespace is
// normalised, so reflowing a long condition is not a change; any other edit is.
var reviewedGateJobConditions = map[string]string{
	"cd.yaml/validate-eks-authorization": "",
	"ci.yaml/heal-prod-on-failure": "always() && github.event_name == 'merge_group' && " +
		"needs.changes.outputs.k8s == 'true' && (needs.deploy-prod.result == 'failure' || " +
		"needs.deploy-prod.result == 'cancelled' || (needs.deploy-prod.result == 'success' && " +
		"needs.merge-group-queue-membership.outputs.evicted == 'true'))",
	"ci.yaml/validate-eks-authorization": "needs.changes.outputs.k8s == 'true' && " +
		"(github.event_name == 'pull_request' || github.event_name == 'merge_group')",
	"dr-rebuild.yaml/rebuild":                       "needs.supersession-gate.outputs.superseded != 'true'",
	"validate-main.yaml/validate-eks-authorization": "",
}

// normalizeCondition collapses every run of whitespace to one space, so a
// reflowed condition compares equal to its reviewed literal.
func normalizeCondition(condition string) string {
	return strings.Join(strings.Fields(condition), " ")
}

// carriesGate reports whether a job runs either gate the coverage guards
// count.
func (j job) carriesGate() bool {
	return j.runsValidator() || j.runsIsolatedChartNamespaceValidator()
}

// gateJobConditionFindings compares every gate-bearing job's condition with
// its reviewed literal. It reports, in a stable order, a gate job with no
// reviewed entry, a condition that differs from its entry (including one that
// was added or removed), and an entry whose job no longer carries a gate — so
// the table cannot keep vouching for a job it no longer describes.
func gateJobConditionFindings(workflows map[string]workflow, reviewed map[string]string) []string {
	var findings []string
	seen := make(map[string]bool, len(reviewed))
	for _, name := range sortedKeys(workflows) {
		for _, jobName := range sortedKeys(workflows[name].Jobs) {
			parsedJob := workflows[name].Jobs[jobName]
			if !parsedJob.carriesGate() {
				continue
			}
			key := name + "/" + jobName
			seen[key] = true
			want, ok := reviewed[key]
			got := normalizeCondition(parsedJob.If)
			switch {
			case !ok:
				findings = append(findings, fmt.Sprintf(
					"%s runs a gate but its condition %q has not been reviewed", key, got))
			case got != normalizeCondition(want):
				findings = append(findings, fmt.Sprintf(
					"%s changed its condition from the reviewed %q to %q", key, normalizeCondition(want), got))
			}
		}
	}
	for _, key := range sortedKeys(reviewed) {
		if !seen[key] {
			findings = append(findings, fmt.Sprintf(
				"%s has a reviewed condition but no longer runs a gate; remove the entry", key))
		}
	}
	return findings
}

// TestGateJobConditionsMatchTheReviewedLiterals fails when a gate-bearing job's
// condition changes without the reviewed entry changing with it.
func TestGateJobConditionsMatchTheReviewedLiterals(t *testing.T) {
	workflows := loadWorkflows(t)
	for _, finding := range gateJobConditionFindings(workflows, reviewedGateJobConditions) {
		t.Errorf("%s. A job-level condition decides whether the gate runs at all, so update "+
			"reviewedGateJobConditions in the same change and say why in the PR.", finding)
	}
	// Fail closed: if the gate detectors stopped matching, every job would be
	// skipped and the loop above would pass over nothing.
	gateJobs := 0
	for _, parsed := range workflows {
		for _, parsedJob := range parsed.Jobs {
			if parsedJob.carriesGate() {
				gateJobs++
			}
		}
	}
	if gateJobs == 0 {
		t.Fatal("no job runs either gate — this guard is now inert")
	}
}

// TestGateJobConditionGuardIsNotVacuous perturbs the real workflows in each
// direction the guard must catch, asserts the perturbation was applied, and
// checks the guard names the job.
func TestGateJobConditionGuardIsNotVacuous(t *testing.T) {
	const conditional = "ci.yaml/validate-eks-authorization"
	const unconditional = "validate-main.yaml/validate-eks-authorization"

	if findings := gateJobConditionFindings(loadWorkflows(t), reviewedGateJobConditions); len(findings) != 0 {
		t.Fatalf("positive control failed: the real workflows must match the table, got %v", findings)
	}

	// perturbed loads a fresh copy of the workflows and rewrites one job's
	// condition. Jobs are map values, so the edit is written back explicitly.
	perturbed := func(t *testing.T, key, condition string) map[string]workflow {
		t.Helper()
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(key, "/")
		parsedJob, ok := workflows[name].Jobs[jobName]
		if !ok {
			t.Fatalf("%s is missing, so this control cannot be applied", key)
		}
		before := parsedJob.If
		parsedJob.If = condition
		workflows[name].Jobs[jobName] = parsedJob
		if normalizeCondition(workflows[name].Jobs[jobName].If) == normalizeCondition(before) {
			t.Fatalf("the perturbation of %s was not applied", key)
		}
		return workflows
	}

	expectFinding := func(t *testing.T, findings []string, key, fragment string) {
		t.Helper()
		for _, finding := range findings {
			if strings.HasPrefix(finding, key+" ") && strings.Contains(finding, fragment) {
				return
			}
		}
		t.Fatalf("want a finding for %s containing %q, got %v", key, fragment, findings)
	}

	t.Run("a changed condition fails", func(t *testing.T) {
		workflows := perturbed(t, conditional, "github.event_name == 'merge_group'")
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			conditional, "changed its condition")
	})

	t.Run("a removed condition fails", func(t *testing.T) {
		workflows := perturbed(t, conditional, "")
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			conditional, `to ""`)
	})

	t.Run("a condition added to an unconditional gate job fails", func(t *testing.T) {
		workflows := perturbed(t, unconditional, "github.repository_owner == 'someone-else'")
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			unconditional, "changed its condition")
	})

	t.Run("a gate job with no reviewed entry fails", func(t *testing.T) {
		reviewed := make(map[string]string, len(reviewedGateJobConditions))
		for key, condition := range reviewedGateJobConditions {
			if key != conditional {
				reviewed[key] = condition
			}
		}
		expectFinding(t, gateJobConditionFindings(loadWorkflows(t), reviewed),
			conditional, "has not been reviewed")
	})

	t.Run("an entry for a job that no longer runs a gate fails", func(t *testing.T) {
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(unconditional, "/")
		parsedJob := workflows[name].Jobs[jobName]
		parsedJob.Steps = nil
		workflows[name].Jobs[jobName] = parsedJob
		if workflows[name].Jobs[jobName].carriesGate() {
			t.Fatalf("the perturbation of %s was not applied", unconditional)
		}
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			unconditional, "no longer runs a gate")
	})

	t.Run("reflowing a condition is not a change", func(t *testing.T) {
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(conditional, "/")
		parsedJob := workflows[name].Jobs[jobName]
		reflowed := strings.ReplaceAll(normalizeCondition(parsedJob.If), " && ", "\n  && ") + "\n"
		if reflowed == parsedJob.If || !strings.Contains(reflowed, "\n  && ") {
			t.Fatalf("the reflow of %s was not applied", conditional)
		}
		parsedJob.If = reflowed
		workflows[name].Jobs[jobName] = parsedJob
		if findings := gateJobConditionFindings(workflows, reviewedGateJobConditions); len(findings) != 0 {
			t.Fatalf("whitespace alone must not count as a change, got %v", findings)
		}
	})
}
