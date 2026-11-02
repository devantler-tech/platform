package refreshfluxghcrauth

import "testing"

// TestPeriodicFanoutUsesControllerRefreshWithoutResourceUpdates exercises the
// complete transaction when the tenant boundary refuses direct refresh writes.
func TestPeriodicFanoutUsesControllerRefreshWithoutResourceUpdates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_PERIODIC_FANOUT_NAMESPACES": "wedding-app ascoachingogvaner",
	})
	requireSuccessResult(t, result)
	requireLine(t, readLines(f.operationLog), "root-patch")
	requireLinesEqual(t, readLines(f.fanoutLog), []string{
		"pushsecret/flux-system/seed-ghcr",
		"externalsecret/data-product-controller/ghcr-auth",
		"externalsecret/kyverno/ghcr-auth",
		"pushsecret/flux-system/seed-ghcr",
		"externalsecret/data-product-controller/ghcr-auth",
		"externalsecret/kyverno/ghcr-auth",
	})
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

// TestPeriodicFanoutRejectsUnprovedRefresh prevents old or unrelated Ready
// evidence from replacing a genuine post-seed refresh and matching credential.
func TestPeriodicFanoutRejectsUnprovedRefresh(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"same-refresh", "replaced-uid", "spec-drift", "unready", "stale-generation", "future-refresh", "read-failure", "wrong-policy", "value-mismatch"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			overrides := map[string]string{
				"FAKE_PERIODIC_FANOUT_NAMESPACES": "wedding-app ascoachingogvaner",
				"FAKE_PERIODIC_FAULT_NAMESPACE":   "wedding-app",
				"FAKE_PERIODIC_FAULT":             fault,
			}
			if fault == "value-mismatch" {
				overrides["FAKE_CONSUMER_MISMATCH_NAMESPACE"] = "wedding-app"
			}
			result := f.runHelper(validConfig(), nil, overrides)
			requireLine(t, readLines(f.operationLog), "periodic-fault:wedding-app:"+fault)
			requireFailureResult(t, result)
			if pathExists(f.patchCapture) || pathExists(f.talosLog) {
				t.Error("root credential or Talos changed after unproved periodic refresh")
			}
			requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
		})
	}
}

// TestMaterializedConsumerReadFailureCannotPass catches a failed API read whose
// stdout nevertheless carries exactly the expected materialized credential.
func TestMaterializedConsumerReadFailureCannotPass(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CONSUMER_READ_FAIL_NAMESPACE": "wedding-app",
	})
	requireFailureResult(t, result)
	if pathExists(f.patchCapture) || pathExists(f.talosLog) {
		t.Error("root credential or Talos changed after a failed consumer read")
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}
