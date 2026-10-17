package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestActualProductionOutcome checks the committed workflow, not only the fixture.
func TestActualProductionOutcome(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductionOutcome(string(data)); err != nil {
		t.Fatal(err)
	}
}

// TestProductionOutcomeScript runs the pinned script for every outcome pair a
// step can report. Only a refused preflight with a skipped composite may tell
// the heal to stand down; every other pair, including the cancelled and empty
// ones a crashed or cancelled job leaves, must write nothing, and so must
// every attempt after the first, whose refusal says nothing about what the
// first attempt deployed.
func TestProductionOutcomeScript(t *testing.T) {
	outcomes := []string{"success", "failure", "cancelled", "skipped", ""}
	for _, attempt := range []string{"1", "2", "10", ""} {
		for _, preflight := range outcomes {
			for _, production := range outcomes {
				output := filepath.Join(t.TempDir(), "output")
				if err := os.WriteFile(output, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("bash", "-euo", "pipefail", "-c", productionOutcomeScript)
				cmd.Env = []string{
					"PATH=" + os.Getenv("PATH"),
					"PREFLIGHT_OUTCOME=" + preflight,
					"PRODUCTION_OUTCOME=" + production,
					"GITHUB_OUTPUT=" + output,
					"GITHUB_RUN_ATTEMPT=" + attempt,
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("attempt=%q preflight=%q production=%q: %v\n%s", attempt, preflight, production, err, out)
				}
				written, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if attempt == "1" && preflight == "failure" && production == "skipped" {
					want = "stopped_before_production=true\n"
				}
				if string(written) != want {
					t.Errorf("attempt=%q preflight=%q production=%q wrote %q, want %q", attempt, preflight, production, written, want)
				}
			}
		}
	}
}

// TestProductionOutcomeMutations rejects every edit that could let the heal
// stand down after a deploy that reached production.
func TestProductionOutcomeMutations(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	outcomeStep := "      - name: 🧾 Record whether production was left untouched"
	for _, tc := range []struct{ name, from, to string }{
		{"output from another step", "${{ steps.production-outcome.outputs.stopped_before_production }}", "${{ steps.queue-preflight.outputs.stopped_before_production }}"},
		{"extra output", "      stopped_before_production: ${{ steps.production-outcome.outputs.stopped_before_production }}", "      stopped_before_production: ${{ steps.production-outcome.outputs.stopped_before_production }}\n      other: 'true'"},
		{"skippable", "        id: production-outcome\n        if: always()", "        id: production-outcome\n        if: success()"},
		{"suppressed", "        id: production-outcome\n", "        id: production-outcome\n        continue-on-error: true\n"},
		{"reads the wrong composite", "${{ steps.production-deploy.outcome }}", "${{ steps.queue-preflight.outcome }}"},
		{"reads a conclusion", "${{ steps.queue-preflight.outcome }}\n          PRODUCTION", "${{ steps.queue-preflight.conclusion }}\n          PRODUCTION"},
		{"vouches for any preflight", `"${PREFLIGHT_OUTCOME}" == 'failure' && `, ``},
		{"vouches for a run composite", `"${PRODUCTION_OUTCOME}" == 'skipped'`, `"${PRODUCTION_OUTCOME}" != 'success'`},
		{"any attempt", `[[ "${GITHUB_RUN_ATTEMPT}" == '1' && `, `[[ `},
		{"step before the preflight", "      - name: 🧾 Refuse an evicted or replaced merge group\n", "      - run: echo earlier-production-step\n\n      - name: 🧾 Refuse an evicted or replaced merge group\n"},
		{"unidentified composite", "        id: production-deploy\n", ""},
		{"not last", outcomeStep, "      - run: echo later-production-step\n\n" + outcomeStep},
		{"step after it", "            printf 'stopped_before_production=true\\n' >>\"${GITHUB_OUTPUT}\"\n          fi\n", "            printf 'stopped_before_production=true\\n' >>\"${GITHUB_OUTPUT}\"\n          fi\n\n      - uses: ./.github/actions/deploy-prod\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(workflow, tc.from, tc.to, 1)
			if mutated == workflow {
				t.Fatal("mutation changed nothing")
			}
			if err := validateProductionOutcome(mutated); err == nil {
				t.Fatal("accepted an outcome step that can vouch for a deploy that ran")
			}
		})
	}
}
