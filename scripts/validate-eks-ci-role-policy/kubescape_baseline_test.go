package main

import (
	"strings"
	"testing"
)

// The kubescape SARIF chain, named by what each half actually does. A workflow
// only counts as publishing the baseline if it RUNS the scan and UPLOADS the
// result — naming either one in a comment is not enough.
const (
	kubescapeScanCommand   = "ksail workload scan"
	kubescapeScanSarifFlag = "--format sarif"
	uploadSarifAction      = "github/codeql-action/upload-sarif"

	// kubescapeCategory is the Code Scanning category the findings are
	// published under. It is part of the contract, not an implementation
	// detail: alerts dedupe and auto-close per (tool, category, ref), so a
	// default-branch upload under a DIFFERENT category would create a second,
	// parallel alert set instead of giving the PR-ref findings a baseline to
	// close against.
	kubescapeCategory = "kubescape-nsa"
)

// scansKubescapeToSarif reports whether this job runs the scan in the
// SARIF-producing mode.
//
// Matched per LINE, and only on a line that actually executes the scan. A
// whole-script `strings.Contains` counts `# ksail workload scan --format sarif`
// in a comment, or an `echo` of the same text, as a scan — so a workflow could
// stop scanning while this guard stayed green, which is the false-green it
// exists to prevent. The file header already claims naming the command in a
// comment is not enough; this is what makes that true.
func (j job) scansKubescapeToSarif() bool {
	for _, s := range j.Steps {
		for _, line := range strings.Split(s.Run, "\n") {
			if isKubescapeScanCommand(line) {
				return true
			}
		}
	}
	return false
}

// isKubescapeScanCommand reports whether ONE script line executes the scan in
// SARIF mode. Deliberately conservative: it rejects rather than guesses, since
// a false positive here re-opens the false-green.
func isKubescapeScanCommand(line string) bool {
	trimmed := strings.TrimSpace(line)
	// A comment executes nothing. This also covers a trailing-comment line whose
	// command half is a comment, which is the shape CodeRabbit raised.
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	// `echo`/`printf` merely PRINT the command text. Strip any leading
	// `VAR=x` assignments and shell prefixes before testing the verb, so
	// `echo ksail workload scan --format sarif` cannot masquerade as a run.
	verb := trimmed
	if i := strings.IndexAny(verb, "|;&"); i >= 0 {
		// Only the segment that would actually invoke ksail matters; check each.
		for _, seg := range strings.FieldsFunc(trimmed, func(r rune) bool {
			return r == '|' || r == ';' || r == '&'
		}) {
			if isKubescapeScanCommand(seg) {
				return true
			}
		}
		return false
	}
	for _, w := range strings.Fields(verb) {
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") {
			continue // leading VAR=value assignment
		}
		if w == "echo" || w == "printf" || w == ":" {
			return false
		}
		break
	}
	return strings.Contains(trimmed, kubescapeScanCommand) &&
		strings.Contains(trimmed, kubescapeScanSarifFlag)
}

// uploadsKubescapeSarif reports whether this job uploads that SARIF to Code
// Scanning under the kubescape category.
func (j job) uploadsKubescapeSarif() bool {
	for _, s := range j.Steps {
		if strings.Contains(s.Uses, uploadSarifAction) && s.With.Category == kubescapeCategory {
			return true
		}
	}
	return false
}

// publishesKubescapeBaselineOnPushToMain reports whether the workflow produces a
// default-branch Kubescape analysis.
//
// Both halves are required of a SINGLE job, not of the workflow as a whole.
// Every job gets its own fresh runner and its own ${RUNNER_TEMP}, so a scan in
// one job and an uploader in another share no filesystem: the uploader would
// have no SARIF to read and no analysis would be published. Matching the two
// halves independently would let exactly that arrangement report the gap as
// closed — the same false-green this guard exists to prevent. A future split
// across jobs is legitimate only with an explicit artifact handoff, which is a
// different shape and should have to update this guard deliberately.
func (w workflow) publishesKubescapeBaselineOnPushToMain() bool {
	if !w.triggersOnPushToMain() {
		return false
	}
	for _, j := range w.Jobs {
		if j.scansKubescapeToSarif() && j.uploadsKubescapeSarif() {
			return true
		}
	}
	return false
}

// TestKubescapeBaselineIsPublishedForTheDefaultBranch pins the gap measured on
// this repository on 2026-07-28: every Kubescape analysis lived on an ephemeral
// `refs/pull/<n>/merge` ref and the repository had ZERO default-branch
// kubescape alerts.
//
// ci.yaml's `validate` job is `pull_request`-only, so merged manifests never
// produced an analysis for the deployed tree. Without a default-branch
// baseline, Code Scanning has nothing to dedupe against and nothing to close:
// the findings exist only for as long as the PR ref does, which defeats the
// automatic-closing behaviour the upload was added to provide (#2829).
func TestKubescapeBaselineIsPublishedForTheDefaultBranch(t *testing.T) {
	workflows := loadWorkflows(t)

	publishing := make([]string, 0, 1)
	for name, parsed := range workflows {
		if parsed.publishesKubescapeBaselineOnPushToMain() {
			publishing = append(publishing, name)
		}
	}

	if len(publishing) == 0 {
		t.Fatalf("no workflow scans manifests and uploads the Kubescape SARIF under "+
			"category %q on push to main — merged manifests produce no default-branch "+
			"analysis, so findings live only on temporary PR refs and can never be "+
			"closed on main. Restore a push-to-main scan-and-upload path.",
			kubescapeCategory)
	}
}

// TestKubescapeBaselineGuardIsNotVacuous proves the guard above can go RED.
//
// Each negative control perturbs exactly ONE of the three conditions the guard
// depends on, so each must flip it to false on its own. Without this, a guard
// that silently stopped matching would report the hole as closed forever —
// which is the same class of failure the baseline gap itself was.
func TestKubescapeBaselineGuardIsNotVacuous(t *testing.T) {
	publishing := workflow{
		On: triggerSet{"push": {Branches: []string{"main"}}},
		Jobs: map[string]job{
			"baseline": {Steps: []step{
				{Run: kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag + " -o out.sarif"},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		},
	}
	if !publishing.publishesKubescapeBaselineOnPushToMain() {
		t.Fatal("positive control failed: a push-to-main workflow that scans and uploads must count")
	}

	t.Run("wrong branch does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.On = triggerSet{"push": {Branches: []string{"release"}}}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a push trigger on another branch must not satisfy default-branch coverage")
		}
	})

	t.Run("pull_request alone does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.On = triggerSet{"pull_request": {Branches: []string{"main"}}}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a pull_request trigger must not satisfy default-branch coverage — " +
				"that is exactly the gap this guard exists to close")
		}
	})

	t.Run("uploading without scanning does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("an upload with no scan producing the SARIF must not count")
		}
	})

	t.Run("scanning without uploading does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a scan whose SARIF is never uploaded must not count — it produces no analysis")
		}
	})

	t.Run("scanning without the SARIF flag does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: kubescapeScanCommand + " --framework nsa"},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a scan that writes no SARIF must not count as producing one")
		}
	})

	t.Run("uploading under another category does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: "some-other-tool"}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("an upload under a different category creates a parallel alert set, " +
				"not a baseline the PR-ref findings can close against")
		}
	})

	// A step's script can NAME the scan without running it. Both shapes below
	// keep every other condition satisfied, so each isolates the executes-vs-
	// mentions distinction alone. Without them the guard reports the baseline as
	// published by a workflow that scans nothing.
	t.Run("a commented-out scan does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: "# " + kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a scan named only in a COMMENT executes nothing and must not count")
		}
	})

	t.Run("an echoed scan command does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: "echo " + kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("an echoed scan command only PRINTS the text and must not count")
		}
	})

	// The complement: a real invocation surrounded by noise must still count, or
	// the tightening above would be a false-negative in the other direction.
	t.Run("a real scan among other lines still counts", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"baseline": {Steps: []step{
				{Run: "# run the scan\n" +
					"echo starting\n" +
					kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag + " -o out.sarif\n"},
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if !perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a genuine scan invocation must still count when a comment and an echo " +
				"of similar text sit beside it")
		}
	})

	t.Run("scan and upload split across jobs does not count", func(t *testing.T) {
		perturbed := publishing
		perturbed.Jobs = map[string]job{
			"scan": {Steps: []step{
				{Run: kubescapeScanCommand + " --framework nsa " + kubescapeScanSarifFlag + " -o out.sarif"},
			}},
			"upload": {Steps: []step{
				{Uses: uploadSarifAction + "@v4", With: stepInputs{Category: kubescapeCategory}},
			}},
		}
		if perturbed.publishesKubescapeBaselineOnPushToMain() {
			t.Fatal("a scan and an upload in SEPARATE jobs must not count: each job gets a " +
				"fresh runner, so the uploader cannot read the scanner's ${RUNNER_TEMP} " +
				"SARIF and no baseline is published — the guard must not report the gap " +
				"closed on a workflow that only names both halves somewhere")
		}
	})
}
