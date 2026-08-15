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
		// A run blocked on the PARENT fence stops before it can reach the
		// child-handoff refusal, so it needs its own pointer or the staged-fence
		// case the report exists for stays undiscoverable.
		"The parent Flux reconciliation is malformed or already suspended",
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

// Every fence outlives the runner that took it, so a node owner built from a
// PID alone is unresolvable exactly like the lease holder was. Without this the
// liveness check reports "no run reference" for precisely the fences the report
// was added to make decidable.
func TestNodeFenceOwnersCarryTheGitHubRunReference(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	for _, owner := range []string{
		`bootstrap_owner="bootstrap-${desired_revision:0:12}-$(fence_run_segment)-$$-${RANDOM}"`,
		`cordon_owner_token="${desired_revision:0:16}-$(fence_run_segment)-$$-${RANDOM}"`,
	} {
		requireContains(t, script, owner)
	}
	// No fence OWNER may be minted without it. Scoped to ownership identities —
	// a reconcile-trigger stamp also ends in PID/RANDOM but is never resolved
	// for liveness, so a blanket count would fail on an unrelated line.
	ownerAssignment := regexp.MustCompile(`(?m)^\s*(?:local\s+)?(\w*(?:owner|owner_token|holder))="([^"]*\$\$[^"]*)"`)
	found := 0
	for _, match := range ownerAssignment.FindAllStringSubmatch(script, -1) {
		found++
		if !strings.Contains(match[2], "fence_run_segment") {
			t.Errorf("fence owner %s is minted without a run reference: %s", match[1], match[2])
		}
	}
	if found != 3 {
		t.Fatalf("fence owner assignments inspected = %d, want 3", found)
	}
}

// The report prints commands an operator pastes against production. A node can
// already be cordoned for maintenance or ill health before the bridge ever
// claimed it, and the journal records that. An unconditional uncordon would
// reverse an intent this script never owned.
func TestFenceReportNeverUncordonsANodeItDidNotCordon(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	report := functionBody(t, script, "report_fences_now")

	// `has`, never `//`: jq's alternative treats a falsy value as empty, which
	// would misreport the one state that may safely uncordon. Checked against
	// the CODE only — the rationale comment names the anti-pattern it forbids.
	requireContains(t, report, `has("wasCordoned")`)
	requireNotContains(t, stepDirectives(report), `.wasCordoned // `)
	// All three states are answered, and only the recorded-safe one uncordons.
	requireContains(t, report, "uncordon")
	for _, state := range []string{"ALREADY cordoned", "UNRECORDED"} {
		requireContains(t, report, state)
	}

	// The branch values must match the journal's OWN schema. wasCordoned is
	// serialized with --argjson and validated as numeric `== 0 or == 1`, so a
	// switch on the booleans matches nothing and every real journal falls to
	// UNRECORDED — the safe uncordon never printed. An earlier version of this
	// test asserted against a hand-written `false` fixture that the code never
	// produces, so it passed over exactly that defect.
	schema := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	requireContains(t, schema, `($record.wasCordoned == 0 or $record.wasCordoned == 1)`)
	directives := stepDirectives(report)
	requireContains(t, directives, "\n        0)\n")
	requireContains(t, directives, "\n        1)\n")
	for _, boolean := range []string{"\n        false)\n", "\n        true)\n"} {
		requireNotContains(t, directives, boolean)
	}
}

// A journal in `active` or `retain` is not releasable by removing annotations:
// reconcile_bootstrap_recovery refuses both, because one may hold an
// interrupted pre-reboot mutation and the other crossed the reboot edge with no
// release-ready proof. Printing removals for them discards the only durable
// record of that state.
func TestFenceReportRefusesToReleasePreProofBootstrapJournals(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	report := functionBody(t, script, "report_fences_now")

	requireContains(t, report, "active | retain)")
	requireContains(t, report, "NOT releasable by annotation removal")
	// Those phases must skip the release block entirely, not merely warn.
	requireContains(t, report, "\n        continue\n")
	// The phases really are the ones the reconciler refuses.
	for _, phase := range []string{`.phase == "active"`, `.phase == "retain"`} {
		requireContains(t, script, phase)
	}
}

// The Lease is the global exclusion fence. Released before the fences it
// guards, it lets a queued or newly dispatched deploy start against a
// half-recovered cluster — so the report must print it last, mirroring
// cleanup_refresh_work's own release order.
func TestFenceReportPrintsTheLeaseLast(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	report := functionBody(t, script, "report_fences_now")

	nodes := requireIndex(t, report, "HELD  Node")
	lease := requireIndex(t, report, "fence_report_lease")
	requireBefore(t, nodes, lease, "node fences reported before the Lease")
	requireContains(t, script, "release LAST, after every fence above is released")
	requireContains(t, report, "Release in the order printed above")
}

// A rerun REUSES the run id and increments the attempt, so `gh run view`
// without --attempt inspects the newest attempt: an orphan from a finished
// attempt reads as live while a later one runs, blocking recovery on a holder
// that is provably dead.
func TestFenceLivenessCommandPinsTheRecordedAttempt(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	liveness := functionBody(t, script, "fence_report_liveness")

	// Assert the flag inside the printed command itself. Checking positions
	// across the whole function would match the rationale comment above it,
	// which names the flag before the command uses it.
	requireContains(t, liveness,
		`gh run view %s --repo devantler-tech/platform --attempt %s --json status,conclusion`)
}

// A failing cluster read is exactly when an operator runs this, so the failure
// path must not exit through the EXIT trap: later cleanup helpers are not
// defined yet at that point and turn the report into a secondary failure.
func TestFenceReportFailurePathStillDisablesTheCleanupTrap(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	// errexit is suspended inside an `if` condition; a bare call is not.
	requireContains(t, script, "if report_fences_now; then")
	requireNotContains(t, script, "\n  report_fences_now\n")
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

// The runbook tells an operator each printed release command is CAS-guarded,
// and the Lease and Kustomization commands are. The node path was not: it
// printed a bare `uncordon` plus one `annotate … -` per annotation. Minutes can
// pass between the report and the paste, and a new transaction can claim the
// node in that window — at which point those commands strip ITS fence and
// uncordon a node it is actively draining, with no error. Test ops close it:
// the API rejects the whole patch when the UID, resourceVersion, owner, or
// journal moved, so losing that race fails loudly instead of silently.
func TestFenceReportNodeReleaseIsCASGuarded(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	report := functionBody(t, script, "report_fences_now")
	command := functionBody(t, script, "fence_node_release_command")

	// The report delegates to the guarded builder and emits no bare mutation of
	// its own. Comments are stripped first: the rationale above the call names
	// the very anti-pattern these assertions forbid.
	requireContains(t, report, "fence_node_release_command")
	directives := stepDirectives(report)
	for _, unguarded := range []string{
		"kubectl --context %s uncordon %s",
		"annotate node %s %s-",
	} {
		requireNotContains(t, directives, unguarded)
	}

	// Every piece of state the report showed the operator is tested before any
	// mutation runs, and it is ONE patch — three separately guarded commands
	// still could not be atomic with each other.
	for _, guard := range []string{
		`{op: "test", path: "/metadata/uid", value: $uid}`,
		`{op: "test", path: "/metadata/resourceVersion", value: $resource_version}`,
		`{op: "test", path: $owner_path, value: $owner}`,
		`{op: "test", path: $recovery_path, value: $recovery}`,
	} {
		requireContains(t, command, guard)
	}
	requireContains(t, command, "patch node %s --type=json")
}
