package refreshfluxghcrauth

import (
	"path/filepath"
	"testing"
)

// A timeout must distinguish the failed predicate without printing the snapshots
// or changing the refusal and fence-cleanup behavior (#4872).
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
			want: "unreadable=2 child_not_owned=0 parent_unstable=0 version_changes=0",
		},
		{
			name:         "child ownership changed",
			env:          map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "child-not-owned"},
			want:         "unreadable=0 child_not_owned=2 parent_unstable=0 version_changes=0",
			foreignChild: true,
		},
		{
			name: "parent reconciling",
			env:  map[string]string{"FAKE_FLUX_POLICY_QUIET_OBSERVATION_STATE": "parent-unstable"},
			want: "unreadable=0 child_not_owned=0 parent_unstable=2 version_changes=0",
		},
		{
			name: "child version keeps changing",
			env:  map[string]string{"FAKE_FLUX_POLICY_RESOURCE_VERSION_CHURN_AFTER_PAUSE": "true"},
			want: "unreadable=0 child_not_owned=0 parent_unstable=0 version_changes=1",
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

func TestQuietWindowSuccessNeedsNoFailureDiagnostic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	requireNotContains(t, result.stdout+result.stderr, "version_changes=")
}
