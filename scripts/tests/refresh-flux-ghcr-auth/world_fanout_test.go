package refreshfluxghcrauth

import (
	"path/filepath"
	"testing"
)

func TestWorldAtRuinFanoutMustConvergeAfterPrepublishDeferral(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	proof := filepath.Join(f.workspace, "runtime-proof.json")
	missing := map[string]string{
		"FAKE_MISSING_FANOUT_RESOURCE": "externalsecret/world-at-ruin/ghcr-auth",
		"FAKE_MISSING_NAMESPACE":       "world-at-ruin",
	}

	staged := f.runHelper(validConfig(), []string{"--record-runtime-proof", proof}, missing)
	requireSuccessResult(t, staged)
	requireContains(t, staged.stdout, "Deferring new GHCR fan-out target world-at-ruin/ghcr-auth")
	requireWrapperPathExists(t, proof, true)
	requireLine(t, readLines(f.fanoutLog), "externalsecret/wedding-app/ghcr-auth")
	requireLine(t, readLines(f.fanoutLog), "externalsecret/kyverno/ghcr-auth")
	requireNoLine(t, readLines(f.fanoutLog), "externalsecret/world-at-ruin/ghcr-auth")
	requireNoLine(t, readLines(f.consumerSecretReadLog), "world-at-ruin/ghcr-auth")

	// The exact staged runtime proof cannot exempt a missing consumer after
	// reconciliation: reuse must fail before changing either source credential.
	strict := f.runHelperPreservingClusterState(validConfig(), []string{"--reuse-runtime-proof", proof}, missing)
	requireFailureResult(t, strict)
	requireContains(t, strict.stdout+strict.stderr, "The GHCR fan-out is incomplete")
	for _, capture := range []string{f.patchCapture, f.variablesPatchCapture, f.fanoutLog, f.talosLog} {
		requireWrapperPathExists(t, capture, false)
	}

	// Model the candidate's namespace and ExternalSecret now existing. The
	// reassertion must force its sync and read its materialized Secret.
	reconciled := f.runHelperPreservingClusterState(validConfig(), []string{"--reuse-runtime-proof", proof}, nil)
	requireSuccessResult(t, reconciled)
	requireLine(t, readLines(f.fanoutLog), "externalsecret/world-at-ruin/ghcr-auth")
	requireLine(t, readLines(f.consumerSecretReadLog), "world-at-ruin/ghcr-auth")
	requireWrapperPathExists(t, f.patchCapture, true)
	requireWrapperSecretAbsent(t, reconciled, "fixture-secret-token")
}

func TestWorldAtRuinActiveConsumerCannotBeDeferredBeforePublish(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), []string{"--record-runtime-proof", filepath.Join(f.workspace, "runtime-proof.json")}, map[string]string{
		"FAKE_MISSING_FANOUT_RESOURCE": "externalsecret/world-at-ruin/ghcr-auth",
		"FAKE_NAMESPACE_WITH_WORKLOAD": "world-at-ruin",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "The GHCR fan-out is incomplete")
	for _, capture := range []string{f.patchCapture, f.variablesPatchCapture, f.fanoutLog, f.talosLog} {
		requireWrapperPathExists(t, capture, false)
	}
}

func TestWorldAtRuinMaterialisedCredentialMustMatchSOPS(t *testing.T) {
	t.Parallel()
	for _, reassert := range []bool{false, true} {
		name := "initial staging"
		if reassert {
			name = "converged reassert"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			current := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}
			if reassert {
				requireSuccessResult(t, f.runHelper(validConfig(), nil, current))
			}
			// The controller reports Ready, but the actual World Secret has a
			// well-formed GHCR credential with a different token than Git/SOPS.
			current["FAKE_CONSUMER_CONFIG_NAMESPACE"] = "world-at-ruin"
			current["FAKE_CONSUMER_DOCKERCONFIGJSON"] = encodeJSON(map[string]any{
				"auths": map[string]any{"ghcr.io": map[string]any{
					"username": "devantler", "password": "fixture-stale-token",
				}},
			})
			var result commandResult
			if reassert {
				result = f.runHelperPreservingClusterState(validConfig(), nil, current)
			} else {
				result = f.runHelper(validConfig(), nil, current)
			}
			requireFailureResult(t, result)
			requireContains(t, result.stdout+result.stderr, "world-at-ruin/ghcr-auth did not materialise")
			requireLine(t, readLines(f.fanoutLog), "externalsecret/world-at-ruin/ghcr-auth")
			requireLine(t, readLines(f.consumerSecretReadLog), "world-at-ruin/ghcr-auth")
			requireWrapperPathExists(t, f.patchCapture, false)
			requireWrapperSecretAbsent(t, result, "fixture-secret-token")
			requireWrapperSecretAbsent(t, result, "fixture-stale-token")
		})
	}
}
