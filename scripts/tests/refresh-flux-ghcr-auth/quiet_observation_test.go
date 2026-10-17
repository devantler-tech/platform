package refreshfluxghcrauth

import (
	"path/filepath"
	"testing"
)

// A timeout must distinguish the failed predicate without printing the snapshots
// or changing the refusal and fence-cleanup behavior (#4872, #4873).
func TestQuietWindowTimeoutExplainsObservedFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		env          map[string]string
		want         string
		foreignChild bool
	}{
		{
			name: "read failure",
			env:  map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "unreadable"},
			want: "unreadable=2 child_not_owned=0 parent_not_owned=0 parent_changed=0 version_changes=0",
		},
		{
			name:         "child ownership changed",
			env:          map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "child-not-owned"},
			want:         "unreadable=0 child_not_owned=2 parent_not_owned=0 parent_changed=0 version_changes=0",
			foreignChild: true,
		},
		{
			name: "parent resumed by another writer",
			env:  map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "parent-not-owned"},
			want: "unreadable=0 child_not_owned=0 parent_not_owned=2 parent_changed=0 version_changes=0",
		},
		{
			name: "parent version keeps changing",
			env:  map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "parent-changed"},
			want: "unreadable=0 child_not_owned=0 parent_not_owned=0 parent_changed=1 version_changes=0",
		},
		{
			name: "child version keeps changing",
			env:  map[string]string{"FAKE_FLUX_POLICY_RESOURCE_VERSION_CHURN_AFTER_PAUSE": "true"},
			want: "unreadable=0 child_not_owned=0 parent_not_owned=0 parent_changed=0 version_changes=1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, tt.env)
			requireFailureResult(t, result)
			output := result.stdout + result.stderr
			requireContains(t, output, "did not acknowledge a stable pause")
			requireContains(t, output, tt.want)
			for _, private := range []string{"fixture-private-owner", "fixture-private-controller-message", "fixture-secret-token"} {
				requireNotContains(t, output, private)
			}
			operations := readLines(f.operationLog)
			for _, forbidden := range []string{"ivpol-policy-apply:verify-app-images", "variables-patch", "root-patch"} {
				requireNoLine(t, operations, forbidden)
			}
			if tt.foreignChild {
				requireNoLine(t, operations, "flux-policy-resume:infrastructure")
				requireNoLine(t, operations, "flux-policy-parent-resume:flux-system")
				if owner := mustReadFile(filepath.Join(f.syncStateDir, "flux-policy-handoff-owner")); owner != "fixture-private-owner" {
					t.Fatalf("foreign fence was changed: owner=%q", owner)
				}
			} else {
				requireLine(t, operations, "flux-policy-resume:infrastructure")
				requireLine(t, operations, "flux-policy-parent-resume:flux-system")
			}
		})
	}
}

// A suspended parent keeps a Reconciling=True condition written by an execution
// the controller restart retired; the new controller never reconciles it again.
// Ownership and an unchanged resourceVersion prove the pause, as for the child,
// so the frozen condition alone must not time the handoff out (#4873).
func TestQuietWindowAcceptsFrozenParentReconcilingCondition(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "parent-frozen-reconciling",
	})
	requireSuccessResult(t, result)
	output := result.stdout + result.stderr
	requireNotContains(t, output, "did not acknowledge a stable pause")
	requireNotContains(t, output, "fixture-private-controller-message")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "ivpol-policy-apply:verify-app-images")
	requireLine(t, operations, "flux-policy-resume:infrastructure")
	requireLine(t, operations, "flux-policy-parent-resume:flux-system")
}

func TestQuietWindowSuccessNeedsNoFailureDiagnostic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	requireNotContains(t, result.stdout+result.stderr, "version_changes=")
}
