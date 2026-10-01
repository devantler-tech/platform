package refreshfluxghcrauth

import (
	"path/filepath"
	"testing"
)

func TestInitialParentReleasedStateIsReclaimedBeforeChildMutation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": "released",
		"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "3",
	})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system")
	if len(claims) != 2 {
		t.Fatalf("parent claims = %d, want initial claim and one reacquisition", len(claims))
	}
	lost := lineIndex(t, operations, "flux-parent-initial-post-claim:released")
	child := lineIndex(t, operations, "flux-policy-pause:infrastructure")
	if claims[0] >= lost || lost >= claims[1] || claims[1] >= child {
		t.Fatalf("unsafe handoff order: claims=%v released=%d child=%d", claims, lost, child)
	}
	for _, marker := range []string{"flux-policy-parent-owner", "flux-policy-parent-suspended", "flux-policy-handoff-owner", "flux-policy-handoff-suspended"} {
		if pathExists(filepath.Join(f.syncStateDir, marker)) {
			t.Fatalf("successful handoff left %s behind", marker)
		}
	}
}

func TestInitialParentUnsafeStateIsRefusedBeforeChildOrCredentialWrites(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"foreign-owner", "replaced", "malformed", "malformed-conditions", "malformed-suspension", "ownerless-suspended", "unsuspended-owned", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": state,
				"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "3",
			})
			requireFailureResult(t, result)
			output := result.stdout + result.stderr
			if state == "unreadable" {
				requireContains(t, output, "Could not inspect the parent Flux reconciliation after claiming")
			} else {
				requireContains(t, output, "parent Flux policy fence changed to an unsafe state after claiming")
			}
			requireNotContains(t, output, "did not quiesce before the image-verification policy handoff")
			requireNotContains(t, output, "fixture-foreign-transaction")
			requireNotContains(t, output, "fixture-secret-token")
			operations := readLines(f.operationLog)
			for _, forbidden := range []string{"flux-policy-pause:infrastructure", "ivpol-policy-apply:verify-app-images", "variables-patch", "root-patch"} {
				requireNoLine(t, operations, forbidden)
			}
			if claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system"); len(claims) != 1 {
				t.Fatalf("unsafe state triggered %d claims, want only the original claim", len(claims))
			}
			if state == "foreign-owner" || state == "replaced" || state == "ownerless-suspended" {
				requireNoLine(t, operations, "flux-policy-parent-resume:flux-system")
			}
		})
	}
}

func TestInitialParentReclaimAdoptsAppliedResponseLossAndRetriesOnlyContention(t *testing.T) {
	t.Parallel()
	for _, patchState := range []string{"response-lost", "churn"} {
		t.Run(patchState, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": "released",
				"FAKE_FLUX_PARENT_RECLAIM_PATCH_STATE":      patchState,
				"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "4",
			})
			requireSuccessResult(t, result)
			operations := readLines(f.operationLog)
			if patchState == "response-lost" {
				if !pathExists(filepath.Join(f.syncStateDir, "flux-parent-reclaim-response-lost")) {
					t.Fatal("fixture did not lose the applied reclaim response")
				}
			} else if rejected := lineIndexes(operations, "flux-parent-reclaim-rejected"); len(rejected) != 1 {
				t.Fatalf("contention injections = %d, want exactly one rejected reclaim", len(rejected))
			}
			claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system")
			if len(claims) != 2 || claims[1] >= lineIndex(t, operations, "flux-policy-pause:infrastructure") {
				t.Fatalf("reclaim not proven before child write: claims=%v", claims)
			}
			requireLine(t, operations, "flux-policy-parent-resume:flux-system")
			if pathExists(filepath.Join(f.syncStateDir, "flux-policy-parent-owner")) {
				t.Fatal("reclaimed parent owner was not cleaned up")
			}
		})
	}
}

func TestInitialParentReclaimsStopAtClaimCapWithinObservationBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": "released",
		"FAKE_FLUX_PARENT_INITIAL_RELEASE_COUNT":    "99",
		"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "8",
		"FLUX_POLICY_PARENT_CLAIM_MAX_ATTEMPTS":     "2",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "observation budget was exhausted after 8 attempts")
	operations := readLines(f.operationLog)
	if claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system"); len(claims) != 3 {
		t.Fatalf("parent claims = %d, want initial claim plus two bounded reclaims", len(claims))
	}
	requireNoLine(t, operations, "flux-policy-pause:infrastructure")
}

func TestInitialParentReclaimRefusesDeniedPatchForeignOwnerAndLostLease(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"denied", "foreign-owner-on-churn", "lease-stolen"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			env := map[string]string{
				"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": "released",
				"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "4",
				"FAKE_FLUX_PARENT_RECLAIM_PATCH_STATE":      scenario,
			}
			if scenario == "lease-stolen" {
				env["FAKE_FLUX_PARENT_LEASE_STOLEN_ON_INITIAL_RELEASE"] = "true"
			}
			result := f.runHelper(validConfig(), nil, env)
			requireFailureResult(t, result)
			operations := readLines(f.operationLog)
			if claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system"); len(claims) != 1 {
				t.Fatalf("unsafe reclaim performed %d successful claims, want only initial claim", len(claims))
			}
			requireNoLine(t, operations, "flux-policy-pause:infrastructure")
			requireNoLine(t, operations, "root-patch")
			requireNoLine(t, operations, "variables-patch")
			wantRejections := 1
			if scenario == "lease-stolen" {
				wantRejections = 0
			}
			if rejected := lineIndexes(operations, "flux-parent-reclaim-rejected"); len(rejected) != wantRejections {
				t.Fatalf("reclaim rejections = %d, want %d for %s", len(rejected), wantRejections, scenario)
			}
		})
	}
}

func TestInitialParentRepeatedReleaseDoesNotResetWaitBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_FLUX_PARENT_INITIAL_POST_CLAIM_STATE": "released",
		"FAKE_FLUX_PARENT_INITIAL_RELEASE_COUNT":    "99",
		"FLUX_GHCR_PARENT_QUIESCE_ATTEMPTS":         "2",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "parent Flux policy handoff observation budget was exhausted")
	operations := readLines(f.operationLog)
	requireNoLine(t, operations, "flux-policy-pause:infrastructure")
	if claims := lineIndexes(operations, "flux-policy-parent-pause:flux-system"); len(claims) != 2 {
		t.Fatalf("parent claims = %d, want only one reacquisition within the two-read budget", len(claims))
	}
}
