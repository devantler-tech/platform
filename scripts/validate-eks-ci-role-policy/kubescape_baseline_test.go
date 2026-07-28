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

// scansKubescapeToSarif reports whether any job actually runs the scan in the
// SARIF-producing mode.
func (w workflow) scansKubescapeToSarif() bool {
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if strings.Contains(s.Run, kubescapeScanCommand) &&
				strings.Contains(s.Run, kubescapeScanSarifFlag) {
				return true
			}
		}
	}
	return false
}

// uploadsKubescapeSarif reports whether any job uploads that SARIF to Code
// Scanning under the kubescape category.
func (w workflow) uploadsKubescapeSarif() bool {
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if strings.Contains(s.Uses, uploadSarifAction) && s.With.Category == kubescapeCategory {
				return true
			}
		}
	}
	return false
}

// publishesKubescapeBaselineOnPushToMain reports whether the workflow produces a
// default-branch Kubescape analysis.
func (w workflow) publishesKubescapeBaselineOnPushToMain() bool {
	return w.triggersOnPushToMain() && w.scansKubescapeToSarif() && w.uploadsKubescapeSarif()
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
}
