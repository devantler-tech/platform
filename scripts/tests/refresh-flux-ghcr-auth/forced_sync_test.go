package refreshfluxghcrauth

import "testing"

// TestForcedRefreshRejectsFailedAPIEvidence catches implicit-errexit fallthrough:
// the real caller handles failures conditionally, so every API result must be
// checked explicitly before old Ready state can establish a completed refresh.
func TestForcedRefreshRejectsFailedAPIEvidence(t *testing.T) {
	t.Parallel()
	const resource = "externalsecret/wedding-app/ghcr-auth"
	for _, fault := range []string{
		"refused-annotation",
		"refused-annotation-with-output",
		"failed-initial-read",
		"failed-completion-read",
		"malformed-initial-read",
		"empty-acknowledgement",
		"malformed-acknowledgement",
		"missing-acknowledgement-version",
		"empty-acknowledgement-version",
		"null-acknowledgement-version",
		"numeric-acknowledgement-version",
	} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				"FAKE_FANOUT_FAULT_RESOURCE": resource,
				"FAKE_FANOUT_FAULT":          fault,
			})
			requireLine(t, readLines(f.operationLog), "fanout-fault:"+resource+":"+fault)
			requireFailureResult(t, result)
			if pathExists(f.patchCapture) {
				t.Error("root credential changed after unproved forced refresh")
			}
			if pathExists(f.talosLog) {
				if operations := readLines(f.talosLog); len(operations) != 0 {
					t.Errorf("Talos mutated after unproved forced refresh: %v", operations)
				}
			}
			requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
		})
	}
}
