package refreshfluxghcrauth

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRevisionFailureDoesNotLeaveOwnedCordonBehind(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
		"FAKE_TALOS_FAIL_OPERATION": "revision",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	uncordon := lineIndex(t, operations, "node-uncordon:prod-worker-1")
	revision := lineIndex(t, operations, "talos-revision:10.0.0.2")
	if uncordon >= revision {
		t.Errorf("uncordon index %d is not before revision index %d", uncordon, revision)
	}
	requireNoLine(t, operations, "root-patch")
}

func TestChangedCordonOwnerIsNeverUncordoned(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_CORDON_OWNER_REPLACED_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "ownership changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireLine(t, operations, "node-drain:prod-worker-1")
	for _, unexpected := range []string{"talos-reboot:10.0.0.2", "node-uncordon:prod-worker-1", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
}

func TestAutoscalerTaintIsNeverUncordoned(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_AUTOSCALER_CORDON_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "scheduling safety state changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireLine(t, operations, "node-drain:prod-worker-1")
	for _, unexpected := range []string{"talos-reboot:10.0.0.2", "node-uncordon:prod-worker-1", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
}

func TestExternalUncordonAfterDrainBlocksReboot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_EXTERNAL_UNCORDON_AFTER_DRAIN_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "scheduling safety state changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "talos-reboot:10.0.0.2")
	requireNoLine(t, operations, "root-patch")
}

func TestChangedInternalIPAfterDrainBlocksReboot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_IP_CHANGED_AFTER_DRAIN_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "identity changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "talos-reboot:10.0.0.2")
	requireNoLine(t, operations, "root-patch")
}

func TestReplacementAfterReadyBlocksImageMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_REPLACED_AFTER_READY_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "identity changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-reboot:10.0.0.2")
	requireNotContains(t, strings.Join(operations, "\n"), "talos-remove:10.0.0.2")
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	requireNoLine(t, operations, "root-patch")
}

func TestExternalUncordonAfterReadyBlocksImageMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_EXTERNAL_UNCORDON_AFTER_READY_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "scheduling safety state changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-reboot:10.0.0.2")
	requireNotContains(t, strings.Join(operations, "\n"), "talos-remove:10.0.0.2")
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	requireNoLine(t, operations, "root-patch")
}

func TestReplacementAfterUncordonBlocksRevisionMarker(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_REPLACED_AFTER_UNCORDON_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "identity changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-uncordon:prod-worker-1")
	requireNoLine(t, operations, "talos-revision:10.0.0.2")
	requireNoLine(t, operations, "root-patch")
}

func TestReplacedNodeIsRejectedBeforeTalosMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_REPLACED_BEFORE_PROCESS_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "identity changed")
	operations := readLines(f.operationLog)
	for _, unexpected := range []string{"talos-auth:10.0.0.2", "node-drain:prod-worker-1", "talos-reboot:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
}

func TestTalosConvergenceBudgetRequiresTargetAndTwoCleanReads(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FLUX_GHCR_TALOS_CONVERGENCE_ATTEMPTS": "2"})
	if result.exitCode != 64 {
		t.Errorf("exit = %d, want 64\n%s%s", result.exitCode, result.stdout, result.stderr)
	}
	requireContains(t, result.stdout+result.stderr, "must be at least 3")
	if pathExists(f.kubectlCalled) {
		t.Error("unsafe convergence budget reached kubectl")
	}
}

func TestUncordonFailureKeepsRevisionMarkerStale(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_UNCORDON_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	for _, unexpected := range []string{"node-uncordon:prod-worker-1", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Error("failed uncordon did not preserve owner marker")
	}
}

func TestUnreadyNodeAfterRebootStopsTheRoll(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_READY_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireLinesEqual(t, readLines(f.operationLog), []string{
		"variables-patch",
		"fanout:pushsecret/flux-system/seed-ghcr",
		"fanout:externalsecret/wedding-app/ghcr-auth",
		"fanout:externalsecret/ascoachingogvaner/ghcr-auth",
		"fanout:externalsecret/kyverno/ghcr-auth",
		"talos-auth:10.0.0.2",
		"node-claim-cordon:prod-worker-1",
		"node-drain:prod-worker-1",
		"talos-reboot:10.0.0.2",
		"node-ready:prod-worker-1",
	})
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func TestUnreadyNodePreservesItsPreExistingCordon(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CORDONED_NODES":       "prod-worker-1",
		"FAKE_NODE_READY_FAIL_NODE": "prod-worker-1",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-ready:prod-worker-1")
	for _, unexpected := range []string{"node-uncordon:prod-worker-1", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func TestTalosFailureAfterSafeFanoutKeepsRootAuthUnchanged(t *testing.T) {
	for _, operation := range []string{"auth", "reboot", "remove", "pull", "revision"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
				"FAKE_TALOS_FAIL_OPERATION": operation,
			})
			requireFailureResult(t, result)
			if !pathExists(f.variablesPatchCapture) {
				t.Error("safe fanout variables patch missing")
			}
			if pathExists(f.patchCapture) {
				t.Error("root credential changed after Talos failure")
			}
			requireLinesEqual(t, readLines(f.fanoutLog), []string{
				"pushsecret/flux-system/seed-ghcr",
				"externalsecret/wedding-app/ghcr-auth",
				"externalsecret/ascoachingogvaner/ghcr-auth",
				"externalsecret/kyverno/ghcr-auth",
			})
			operations := readLines(f.operationLog)
			if operation == "remove" || operation == "pull" {
				requireNoLine(t, operations, "node-uncordon:prod-worker-1")
				if !pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
					!pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
					t.Error("cache proof failure did not preserve the owned cordon")
				}
			}
			if operation == "reboot" {
				requireNoLine(t, operations, "node-uncordon:prod-worker-1")
			}
			requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
		})
	}
}
