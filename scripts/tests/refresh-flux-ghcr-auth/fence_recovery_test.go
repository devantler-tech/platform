package refreshfluxghcrauth

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The fence report is the supported answer to "prove the prior process is dead
// before explicitly recovering". It must run when a deploy is already refusing
// to start, so it may not depend on the credential path, and it must never
// mutate: releasing a fence is the operator's explicit step, because a Talos
// write cannot be fenced and a surviving holder could still be alive.
func TestFenceReportRunsReadOnlyAndWithoutTheCredentialPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	result := f.runHelper(validConfig(), []string{"--fences"}, nil)

	requireSuccessResult(t, result)
	if !strings.Contains(result.stdout, "GHCR deploy fences") {
		t.Errorf("fence report missing its header; stdout = %q", result.stdout)
	}
	if _, err := os.Stat(f.patchCapture); !os.IsNotExist(err) {
		t.Errorf("fence report wrote the root credential patch (%v); it must not touch the credential path", err)
	}
	for _, forbidden := range []string{"::error::", "command not found"} {
		if strings.Contains(result.stdout+result.stderr, forbidden) {
			t.Errorf("fence report emitted %q; output = %q", forbidden, result.stdout+result.stderr)
		}
	}
}

func TestFenceReportRejectsUnknownFlagsAndAdvertisesItself(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	result := f.runHelper(validConfig(), []string{"--not-a-flag"}, nil)

	if result.exitCode != 64 {
		t.Errorf("unknown flag exit = %d, want 64", result.exitCode)
	}
	if !strings.Contains(result.stderr, "--fences") {
		t.Errorf("usage does not advertise --fences; stderr = %q", result.stderr)
	}
}

// A PID belongs to a runner that no longer exists, so an identity built only
// from one cannot be checked for liveness. Every fence reuses the lease holder,
// so recording the run reference once makes all of them decidable.
func TestFenceHolderIdentityCarriesTheGitHubRunReference(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	requireContains(t, script, `sync_lease_holder="${desired_revision:0:16}-$(fence_run_segment)-$$-${RANDOM}"`)
	requireContains(t, script, `printf 'gh%s.%s' "${GITHUB_RUN_ID}" "${GITHUB_RUN_ATTEMPT:-1}"`)
	// Both policy fences derive from the lease holder, so they inherit it.
	requireContains(t, script, `flux_policy_parent_owner="${sync_lease_holder}"`)
	requireContains(t, script, `flux_policy_handoff_owner="${sync_lease_holder}"`)
}

// A refusal that names no recovery route is what turned this class of stall
// into archaeology twice on 2026-08-09.
func TestFenceRefusalsPointAtTheReportAndTheRunbook(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	runbook := readRepositoryFile(t, "docs/dr/runbook.md")

	refusals := []string{
		"holds the synchronization lease",
		"already owns the image-verification policy handoff",
	}
	for _, refusal := range refusals {
		index := strings.Index(script, refusal)
		if index < 0 {
			t.Fatalf("refusal %q not found", refusal)
		}
		line := script[index:]
		if end := strings.Index(line, "\n"); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, "--fences") {
			t.Errorf("refusal %q does not name the fence report: %s", refusal, line)
		}
		if !strings.Contains(line, "runbook.md") {
			t.Errorf("refusal %q does not name the runbook: %s", refusal, line)
		}
	}

	requireContains(t, runbook, "Recover an orphaned GHCR deploy fence")
	// The recovery procedure is worthless if it does not stop the operator
	// releasing a fence that a running deploy legitimately holds.
	requireContains(t, runbook, "gh run view <run-id>")
}

// jsonPatchOps returns the {op, path} pairs of every JSON Patch operation in a
// snippet, normalized so a release command can be compared against the release
// function it must mirror.
func jsonPatchOps(snippet string) []string {
	pattern := regexp.MustCompile(`\{op:\s*"([a-z]+)",\s*path:\s*(\$?[A-Za-z_/."]+)`)
	matches := pattern.FindAllStringSubmatch(snippet, -1)
	ops := make([]string, 0, len(matches))
	for _, match := range matches {
		ops = append(ops, match[1]+" "+strings.Trim(match[2], `"`))
	}
	sort.Strings(ops)
	return ops
}

func functionBody(t *testing.T, script, name string) string {
	t.Helper()
	start := requireIndex(t, script, "\n"+name+"() {")
	rest := script[start+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("no closing brace for %s", name)
	}
	return rest[:end]
}

// The printed recovery command is only useful if it mirrors the release the
// script itself performs. The parent fence carries NO `reconcile: disabled` —
// only the child handoff does — so emitting that test op for the parent makes
// the whole patch fail its own precondition, and the operator cannot release
// the root Kustomization at all. Compare the op sets rather than asserting on
// substrings: the defect was a mismatch, so a mismatch is what must be caught.
func TestFenceReleaseCommandsMirrorTheReleaseTheyStandInFor(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	command := functionBody(t, script, "fence_kustomization_release_command")
	split := strings.Index(command, "\n  else\n")
	if split < 0 {
		t.Fatal("expected fence_kustomization_release_command to branch child vs parent")
	}
	childCommand, parentCommand := command[:split], command[split:]

	cases := []struct {
		name    string
		emitted string
		release string
	}{
		{"child handoff", childCommand, functionBody(t, script, "resume_flux_policy_handoff")},
		{"parent", parentCommand, functionBody(t, script, "resume_flux_policy_parent")},
	}
	for _, testCase := range cases {
		emitted := jsonPatchOps(testCase.emitted)
		release := jsonPatchOps(testCase.release)
		if len(emitted) == 0 || len(release) == 0 {
			t.Fatalf("%s: parsed no operations (emitted=%d release=%d)",
				testCase.name, len(emitted), len(release))
		}
		if !reflect.DeepEqual(emitted, release) {
			t.Errorf("%s: recovery command does not mirror its release function\n  command: %v\n  release: %v",
				testCase.name, emitted, release)
		}
	}
}

// A node whose cordon was claimed by the ordinary per-node path carries the
// OWNER annotation and no recovery journal, so keying the sweep on the journal
// reports "no fence held" while that node stays cordoned and the next run
// refuses its owner — a false all-clear, the worst failure for this tool.
func TestFenceReportSweepsCordonOwnerNotOnlyTheRecoveryJournal(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	report := functionBody(t, script, "report_fences_now")
	requireContains(t, report, "CORDON_OWNER_ANNOTATION")
	requireContains(t, report, `select($owner != "" or $recovery != "")`)

	// The ordinary path really does claim ownership with an empty journal —
	// if that ever stops being true this test should be revisited, not deleted.
	requireContains(t, script, `      "" "${was_cordoned}" "${initial_node_taints}" || return 1`)
}
