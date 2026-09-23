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
	for _, operation := range readLines(f.operationLog) {
		switch operation {
		case "variables-patch", "root-patch", "ivpol-policy-apply:verify-app-images":
			t.Errorf("no-drift reassert wrote cluster state: %s", operation)
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
