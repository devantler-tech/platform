package refreshfluxghcrauth

import (
	"os"
	"path/filepath"
	"testing"
)

// The bridge fences Flux before it mutates: it pauses the parent and child
// policy reconciliations and restarts kustomize-controller, because Flux
// documents that suspending does not stop an execution that already started.
// That fence is the dominant cost of a deploy which writes nothing, and the
// script runs twice per deploy (platform#3039).
//
// These two tests pin BOTH directions, which is the whole safety property: the
// fence is skipped only when a read-only probe proves every endpoint already
// matches Git/SOPS, and it is still taken the moment anything drifts.

func fluxControllerRestartCount(t *testing.T, f *fixture) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(f.syncStateDir, "flux-controller-restart-count"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read kustomize-controller restart count: %v", err)
	}
	return string(content)
}

func TestFullyCurrentClusterSkipsTheFluxFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"})
	requireSuccessResult(t, result)

	if restarts := fluxControllerRestartCount(t, f); restarts != "" {
		t.Errorf("a cluster with nothing to write still restarted kustomize-controller (count %q)", restarts)
	}
	if pathExists(f.talosLog) {
		t.Error("a cluster with nothing to write still reached the Talos API")
	}
	// Root auth is still reasserted from Git/SOPS. Skipping the fence must not
	// weaken the documented "reasserts root auth on every run" property — only
	// the machinery that exists to make a MUTATION safe is skipped.
	if !pathExists(f.patchCapture) {
		t.Error("fast path skipped the root Flux auth reassert")
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

// The escalation case, and the reason `--check-only` could not have been reused
// for this: it returns after proving the SOPS credential pulls from GHCR on the
// runner, which stays true while a node's machine config is only partly
// applied. Here the credential revision matches but the verified image does
// not — exactly that partial state — so the probe must refuse the fast path and
// the full fenced transaction must run.
func TestPartiallyAppliedNodeConfigStillTakesTheFluxFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":  "true",
		"FAKE_TALOS_VERIFIED_IMAGE": "ghcr.io/devantler-tech/ksail:v7.166.0",
	})
	requireSuccessResult(t, result)

	if restarts := fluxControllerRestartCount(t, f); restarts == "" {
		t.Error("drifted node proof took the fast path; the fenced transaction never ran")
	}
	if !pathExists(f.talosLog) {
		t.Error("drifted node proof never reached the Talos API")
	}
	if !pathExists(f.patchCapture) {
		t.Error("root patch missing after the fenced transaction")
	}
}
