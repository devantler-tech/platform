package refreshfluxghcrauth

import (
	"os"
	"path/filepath"
	"testing"
)

func persistedClusterMarker(t *testing.T, f *fixture, name string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(f.syncStateDir, name))
	if err != nil {
		t.Fatalf("read cluster marker %s: %v", name, err)
	}
	return string(value)
}

func TestConvergedReassertKeepsFullVerificationWithoutWriting(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	current := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}

	requireSuccessResult(t, f.runHelper(validConfig(), nil, current))
	restarts := persistedClusterMarker(t, f, "flux-controller-restart-count")
	leaseVersion := persistedClusterMarker(t, f, "sync-lease-resource-version")

	result := f.runHelperPreservingClusterState(validConfig(), nil, current)
	requireSuccessResult(t, result)
	if got := persistedClusterMarker(t, f, "flux-controller-restart-count"); got != restarts {
		t.Errorf("no-drift reassert restarted kustomize-controller: %s -> %s", restarts, got)
	}
	if got := persistedClusterMarker(t, f, "sync-lease-resource-version"); got != leaseVersion {
		t.Errorf("no-drift reassert acquired the synchronization Lease: %s -> %s", leaseVersion, got)
	}
	// Every write appends to the operation log, so a run that wrote nothing
	// leaves no log at all.
	if pathExists(f.operationLog) {
		for _, operation := range readLines(f.operationLog) {
			switch operation {
			case "variables-patch", "root-patch", "ivpol-policy-apply:verify-app-images":
				t.Errorf("no-drift reassert wrote cluster state: %s", operation)
			}
		}
	}
	if pathExists(f.fanoutLog) {
		t.Error("no-drift reassert forced an External Secrets sync")
	}
	if pathExists(f.talosLog) {
		t.Error("no-drift reassert changed Talos state")
	}
	if !pathExists(f.kubectlCalled) {
		t.Error("no-drift reassert skipped live cluster verification")
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func TestPartiallyAppliedNodeConfigStillAcquiresFullFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	requireSuccessResult(t, f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT": "true",
	}))
	restarts := persistedClusterMarker(t, f, "flux-controller-restart-count")

	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":  "true",
		"FAKE_TALOS_VERIFIED_IMAGE": "ghcr.io/devantler-tech/ksail:v7.166.0",
	})
	requireSuccessResult(t, result)
	if got := persistedClusterMarker(t, f, "flux-controller-restart-count"); got == restarts {
		t.Error("node-level image drift bypassed the Flux fence")
	}
	if !pathExists(f.talosLog) {
		t.Error("node-level image drift bypassed Talos verification")
	}
}

func TestStaleOpenBaoSeedCannotTakeTheNoWritePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	current := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}
	requireSuccessResult(t, f.runHelper(validConfig(), nil, current))
	correctSeed := persistedClusterMarker(t, f, "vault-seed-value")
	if err := os.WriteFile(filepath.Join(f.syncStateDir, "vault-seed-value"), []byte("stale-after-raft-restore"), 0o600); err != nil {
		t.Fatalf("model restored OpenBao seed: %v", err)
	}
	restarts := persistedClusterMarker(t, f, "flux-controller-restart-count")

	result := f.runHelperPreservingClusterState(validConfig(), nil, current)
	requireSuccessResult(t, result)
	if got := persistedClusterMarker(t, f, "vault-seed-value"); got != correctSeed {
		t.Error("reassert left a stale remote OpenBao seed while Kubernetes consumers appeared current")
	}
	if got := persistedClusterMarker(t, f, "flux-controller-restart-count"); got == restarts {
		t.Error("reassert repaired the remote seed without acquiring the Flux fence")
	}
}

// Each read-only convergence condition must, on its own, keep a no-drift
// reassert off the no-write path. Where the fenced path can repair the state,
// it must also have run. A state the fenced path itself refuses (a suspended
// parent it does not own, an unreconciled consumer it cannot prove) only has to
// stay off the no-write path.
func TestUnprovedConvergenceTakesTheFullFence(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env        map[string]string
		fencedPath bool
	}{
		"stale seed probe":   {map[string]string{"FAKE_SEED_PROBE_STALE": "true"}, true},
		"unready seed probe": {map[string]string{"FAKE_SEED_PROBE_NOT_READY": "true"}, true},
		"missing seed probe": {map[string]string{"FAKE_SEED_PROBE_MISSING": "true"}, true},
		"seed probe not refreshed since the run began": {map[string]string{
			"FAKE_SEED_PROBE_REFRESHED_BEFORE_RUN": "true",
			"FLUX_GHCR_SEED_PROBE_WAIT_SECONDS":    "3",
		}, true},
		"drifted admission": {map[string]string{"FAKE_IMAGE_VERIFICATION_POLICY_DRIFTED": "true"}, true},
		"narrowed webhook":  {map[string]string{"FAKE_IMAGE_VERIFICATION_WEBHOOK_SCOPE_NARROWED": "true"}, true},
		"failed read of the retired policy": {map[string]string{
			"FAKE_RETIRED_IMAGE_VERIFICATION_POLICY_READ_FAILS": "true",
		}, true},
		"lease claimed during the checks": {map[string]string{
			"FAKE_SYNC_LEASE_CLAIMED_DURING_CONVERGENCE": "true",
		}, true},
		"unreconciled consumer": {map[string]string{
			"FAKE_UNRECONCILED_FANOUT_RESOURCE": "externalsecret/kyverno/ghcr-auth",
		}, false},
		"unreconciled seed PushSecret": {map[string]string{
			"FAKE_UNRECONCILED_FANOUT_RESOURCE": "pushsecret/flux-system/seed-ghcr",
		}, false},
		"suspended parent Kustomization": {map[string]string{
			"FAKE_FLUX_POLICY_PARENT_SUSPENDED_UNOWNED": "true",
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			requireSuccessResult(t, f.runHelper(validConfig(), nil, map[string]string{
				"FAKE_TALOS_NODES_CURRENT": "true",
			}))
			restarts := persistedClusterMarker(t, f, "flux-controller-restart-count")

			overrides := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}
			for key, value := range tc.env {
				overrides[key] = value
			}
			result := f.runHelperPreservingClusterState(validConfig(), nil, overrides)
			requireNotContains(t, result.stdout, "no fence or write was needed")
			if !tc.fencedPath {
				return
			}
			requireSuccessResult(t, result)
			if got := persistedClusterMarker(t, f, "flux-controller-restart-count"); got == restarts {
				t.Errorf("%s took the no-write path", name)
			}
		})
	}
}
