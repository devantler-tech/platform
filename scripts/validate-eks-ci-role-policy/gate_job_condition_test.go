package main

import (
	"fmt"
	"strings"
	"testing"
)

// reviewedGateJobConditions pins the `if:` of every job that decides whether
// one of the gates the coverage guards in this package look for runs, keyed
// `<workflow>/<job>`: the gate jobs themselves and every job they need,
// transitively (see gateDecidingJobs for why, and where the walk stops).
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
	"cd.yaml/validate-pvc-prune-safety":  "",
	"cd.yaml/validate-rgd-templates":     "",
	"ci.yaml/changes":                    "",
	"dr-rebuild.yaml/supersession-gate":  "",
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

// needsOf lists the jobs a job depends on. Actions accepts `needs:` as a single
// job name or a list of them; any other shape is reported as unreadable so the
// caller fails closed instead of treating the job as having no dependencies.
func (j job) needsOf() ([]string, bool) {
	switch needs := j.Needs.(type) {
	case nil:
		return nil, true
	case string:
		return []string{needs}, true
	case []any:
		names := make([]string, 0, len(needs))
		for _, need := range needs {
			name, ok := need.(string)
			if !ok {
				return nil, false
			}
			names = append(names, name)
		}
		return names, true
	}
	return nil, false
}

// runsDespiteSkippedNeeds reports whether a job's condition calls one of the
// status functions that replace the implicit `success()` check. Only such a job
// runs when a job it needs was skipped, so only there does an upstream job's
// condition stop deciding whether this one runs.
func (j job) runsDespiteSkippedNeeds() bool {
	for _, status := range []string{"always()", "failure()", "cancelled()"} {
		if strings.Contains(j.If, status) {
			return true
		}
	}
	return false
}

// gateDecidingJobs returns the jobs whose condition decides whether a gate job
// in this workflow runs: every gate job, plus every job it needs, transitively.
// A job that `needs:` a skipped job is itself skipped unless its condition
// calls a status function, so pinning only the gate job's own `if:` would let
// an `if: false` on any upstream job switch the gate off unseen. The walk stops
// at a job that runs despite skipped needs: an upstream job being skipped no
// longer skips it implicitly. Its own condition is still pinned, and it can
// still read an upstream job's result or outputs — heal-prod-on-failure does —
// so an upstream `if: false` can still stop it. That stops its deploy along
// with its gate, so it cannot let anything reach production ungated. A
// dependency that cannot be read, or that names no job in the workflow, is
// returned as a finding.
func gateDecidingJobs(name string, parsed workflow) (map[string]bool, []string) {
	deciding := make(map[string]bool)
	var findings []string
	var visit func(jobName string)
	visit = func(jobName string) {
		if deciding[jobName] {
			return
		}
		deciding[jobName] = true
		parsedJob := parsed.Jobs[jobName]
		if parsedJob.runsDespiteSkippedNeeds() {
			return
		}
		needs, ok := parsedJob.needsOf()
		if !ok {
			findings = append(findings, fmt.Sprintf(
				"%s/%s has a `needs:` this guard cannot read, so the jobs deciding whether its gate runs are unknown",
				name, jobName))
			return
		}
		for _, need := range needs {
			if _, exists := parsed.Jobs[need]; !exists {
				findings = append(findings, fmt.Sprintf(
					"%s/%s needs %q, which is not a job in this workflow", name, jobName, need))
				continue
			}
			visit(need)
		}
	}
	for _, jobName := range sortedKeys(parsed.Jobs) {
		if parsed.Jobs[jobName].carriesGate() {
			visit(jobName)
		}
	}
	return deciding, findings
}

// gateJobConditionFindings compares the condition of every job that decides
// whether a gate runs with its reviewed literal. It reports, in a stable
// order, such a job with no reviewed entry, a condition that differs from its
// entry (including one that was added or removed), and an entry whose job no
// longer decides a gate — so the table cannot keep vouching for a job it no
// longer describes.
func gateJobConditionFindings(workflows map[string]workflow, reviewed map[string]string) []string {
	var findings []string
	seen := make(map[string]bool, len(reviewed))
	for _, name := range sortedKeys(workflows) {
		deciding, walkFindings := gateDecidingJobs(name, workflows[name])
		findings = append(findings, walkFindings...)
		for _, jobName := range sortedKeys(deciding) {
			key := name + "/" + jobName
			seen[key] = true
			want, ok := reviewed[key]
			got := normalizeCondition(workflows[name].Jobs[jobName].If)
			switch {
			case !ok:
				findings = append(findings, fmt.Sprintf(
					"%s decides whether a gate runs but its condition %q has not been reviewed", key, got))
			case got != normalizeCondition(want):
				findings = append(findings, fmt.Sprintf(
					"%s changed its condition from the reviewed %q to %q", key, normalizeCondition(want), got))
			}
		}
	}
	for _, key := range sortedKeys(reviewed) {
		if !seen[key] {
			findings = append(findings, fmt.Sprintf(
				"%s has a reviewed condition but no longer decides whether a gate runs; remove the entry", key))
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
	// skipped and the loop above would pass over nothing. The real floor is the
	// table itself — a listed job the detectors stop matching is reported as a
	// stale entry — so this only catches the table and detectors both emptying.
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

	t.Run("a condition added to a job a gate job needs fails", func(t *testing.T) {
		const upstream = "ci.yaml/changes"
		workflows := perturbed(t, upstream, "false")
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			upstream, "changed its condition")
	})

	t.Run("an unreadable needs fails", func(t *testing.T) {
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(conditional, "/")
		parsedJob := workflows[name].Jobs[jobName]
		parsedJob.Needs = map[string]any{"changes": true}
		workflows[name].Jobs[jobName] = parsedJob
		if _, ok := workflows[name].Jobs[jobName].needsOf(); ok {
			t.Fatalf("the perturbation of %s was not applied", conditional)
		}
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			conditional, "cannot read")
	})

	t.Run("a needs naming no job fails", func(t *testing.T) {
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(conditional, "/")
		parsedJob := workflows[name].Jobs[jobName]
		parsedJob.Needs = []any{"no-such-job"}
		workflows[name].Jobs[jobName] = parsedJob
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			conditional, "not a job in this workflow")
	})

	t.Run("the walk stops at a job that runs despite skipped needs", func(t *testing.T) {
		parsed := workflow{Jobs: map[string]job{
			"upstream": {If: "false"},
			"gate": {
				Needs: "upstream",
				If:    "always()",
				Steps: []step{{Run: validatorInvocation + " ."}},
			},
		}}
		deciding, findings := gateDecidingJobs("fixture.yaml", parsed)
		if len(findings) != 0 || !deciding["gate"] || deciding["upstream"] {
			t.Fatalf("an always() gate job is not decided by its needs; got deciding=%v findings=%v",
				deciding, findings)
		}
		parsed.Jobs["gate"] = job{Needs: "upstream", Steps: parsed.Jobs["gate"].Steps}
		deciding, _ = gateDecidingJobs("fixture.yaml", parsed)
		if !deciding["upstream"] {
			t.Fatal("without a status function the gate job is skipped with its need, so the need must be pinned")
		}
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

	t.Run("an entry for a job that no longer decides a gate fails", func(t *testing.T) {
		workflows := loadWorkflows(t)
		name, jobName, _ := strings.Cut(unconditional, "/")
		parsedJob := workflows[name].Jobs[jobName]
		parsedJob.Steps = nil
		workflows[name].Jobs[jobName] = parsedJob
		if workflows[name].Jobs[jobName].carriesGate() {
			t.Fatalf("the perturbation of %s was not applied", unconditional)
		}
		expectFinding(t, gateJobConditionFindings(workflows, reviewedGateJobConditions),
			unconditional, "no longer decides whether a gate runs")
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
