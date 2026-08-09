package refreshfluxghcrauth

import (
	"os"
	"path/filepath"
	"testing"
)

// The bridge fences Flux before it mutates: it pauses the parent and child
// policy reconciliations and restarts kustomize-controller, because Flux
// documents that suspending does not stop an execution that already started.
// That fence is correct whenever something is written, and it is the dominant
// cost of a deploy that writes nothing — the script runs twice per deploy, and
// the second invocation measured 62 s of a 276 s deploy while reporting no
// change on both Secrets (platform#3039).
//
// Both tests run the script TWICE against preserved cluster state, which is the
// production shape: the deploy stages before publishing and reasserts after
// `ksail cluster update`. They pin both directions of the safety property — the
// fence is skipped only once a read-only probe proves every endpoint already
// matches Git/SOPS, and is still taken the moment anything drifts.

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

func TestReassertOverAConvergedClusterSkipsTheFluxFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	current := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}

	requireSuccessResult(t, f.runHelper(validConfig(), nil, current))
	afterStage := fluxControllerRestartCount(t, f)
	if afterStage == "" {
		t.Fatal("the converging run did not fence Flux; the fixture is not exercising the mutation path")
	}

	result := f.runHelperPreservingClusterState(validConfig(), nil, current)
	requireSuccessResult(t, result)

	if after := fluxControllerRestartCount(t, f); after != afterStage {
		t.Errorf("reassert over a converged cluster restarted kustomize-controller again (%q -> %q)", afterStage, after)
	}
	if pathExists(f.talosLog) {
		t.Error("reassert over a converged cluster reached the Talos API")
	}
	// Root auth is still reasserted from Git/SOPS. Skipping the fence must not
	// weaken the documented "reasserts root auth on every run" property — only
	// the machinery that exists to make a MUTATION safe is skipped.
	if !pathExists(f.patchCapture) {
		t.Error("fast path skipped the root Flux auth reassert")
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

// The escalation case, and the reason `--check-only` could not be reused for
// this: it returns after proving the SOPS credential pulls from GHCR on the
// runner, which stays true while a node's machine config is only partly
// applied. Here the credential revision matches but the verified image does
// not — exactly that partial state — so the probe must refuse the fast path and
// the full fenced transaction must run.
func TestReassertWithPartiallyAppliedNodeConfigStillFences(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	requireSuccessResult(t, f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT": "true",
	}))
	afterStage := fluxControllerRestartCount(t, f)

	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":  "true",
		"FAKE_TALOS_VERIFIED_IMAGE": "ghcr.io/devantler-tech/ksail:v7.166.0",
	})
	requireSuccessResult(t, result)

	if after := fluxControllerRestartCount(t, f); after == afterStage {
		t.Errorf("drifted node proof took the fast path; the fenced transaction never ran (count stayed %q)", afterStage)
	}
	if !pathExists(f.talosLog) {
		t.Error("drifted node proof never reached the Talos API")
	}
	if !pathExists(f.patchCapture) {
		t.Error("root patch missing after the fenced transaction")
	}
}

// CodeRabbit raised this on #3041 and it is a real window: the probe reads the
// root Secret, then the fast path patches it OUTSIDE the fence. A writer that
// lands in between would have its change overwritten by a decision made against
// a cluster that no longer exists. The fast path therefore pins the observed
// resourceVersion, and anything else falls through to the fenced transaction.
func TestRootSecretMovingAfterTheProbeFallsBackToTheFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	current := map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}

	requireSuccessResult(t, f.runHelper(validConfig(), nil, current))
	afterStage := fluxControllerRestartCount(t, f)

	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":           "true",
		"FAKE_ROOT_SECRET_MOVES_AFTER_PROBE": "true",
	})
	requireSuccessResult(t, result)

	if after := fluxControllerRestartCount(t, f); after == afterStage {
		t.Errorf("root Secret moved after the probe but the run still took the unfenced fast path (count stayed %q)", afterStage)
	}
}
