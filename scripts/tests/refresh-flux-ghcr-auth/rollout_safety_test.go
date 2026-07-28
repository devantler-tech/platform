package refreshfluxghcrauth

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevisionFailureKeepsOwnedCordonAndBlocksRetry(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
		"FAKE_TALOS_FAIL_OPERATION": "revision",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-revision:10.0.0.2")
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	if !pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
		!pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Fatal("revision-marker failure did not retain the owned cordon")
	}
	requireNoLine(t, operations, "root-patch")

	retry := f.runHelperPreservingClusterState(validConfig(), nil, nil)
	requireFailureResult(t, retry)
	requireContains(t, retry.stdout+retry.stderr, "Could not select Talos nodes requiring GHCR synchronization")
	if pathExists(f.talosLog) {
		t.Fatal("retry mutated Talos while residual bridge ownership remained")
	}
}

func TestCredentialPatchOccursUnderOwnedCordon(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	claim := lineIndex(t, operations, "node-claim-cordon:prod-worker-1")
	auth := lineIndex(t, operations, "talos-auth:10.0.0.2")
	if claim >= auth {
		t.Fatalf("credential patch was not protected by cordon ownership: claim=%d auth=%d", claim, auth)
	}
}

func TestLiveSynchronizationLeaseBlocksEveryMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_HELD_SYNC_LEASE": "true",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "holds the synchronization lease")
	if pathExists(f.variablesPatchCapture) || pathExists(f.patchCapture) || pathExists(f.talosLog) {
		t.Fatal("live synchronization lease did not block every cluster mutation")
	}
}

func TestExpiredSynchronizationLeaseRequiresExplicitRecovery(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_HELD_SYNC_LEASE":    "true",
		"FAKE_EXPIRED_SYNC_LEASE": "true",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "holds the synchronization lease")
	if pathExists(f.variablesPatchCapture) || pathExists(f.patchCapture) || pathExists(f.talosLog) {
		t.Fatal("expired synchronization lease was taken over without explicit recovery")
	}
}

func TestSameHolderLeaseRenewalRaceDoesNotAbortTheTransaction(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_SYNC_LEASE_RENEW_CONFLICT_ONCE": "true",
	})
	requireSuccessResult(t, result)
	if !pathExists(filepath.Join(f.syncStateDir, "sync-lease-renew-conflict")) {
		t.Fatal("fixture did not exercise the same-holder lease renewal conflict")
	}
	requireLine(t, readLines(f.operationLog), "root-patch")
}

func TestCurrentLeaseHolderRetriesSecretCASConflicts(t *testing.T) {
	for name, test := range map[string]struct {
		environment string
		marker      string
		operation   string
		liveValue   string
	}{
		"root Secret": {
			environment: "FAKE_ROOT_SECRET_CAS_CONFLICT_ONCE",
			marker:      "root-secret-cas-conflict",
			operation:   "root-patch",
			liveValue:   "newer-root-secret-value",
		},
		"variables-base Secret": {
			environment: "FAKE_VARIABLES_SECRET_CAS_CONFLICT_ONCE",
			marker:      "variables-secret-cas-conflict",
			operation:   "variables-patch",
			liveValue:   "newer-variables-secret-value",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				test.environment: "true",
			})
			requireSuccessResult(t, result)
			if !pathExists(filepath.Join(f.syncStateDir, test.marker)) {
				t.Fatal("fixture did not exercise the concurrent stale Secret writer")
			}
			requireLine(t, readLines(f.operationLog), test.operation)
			capturePath := f.patchCapture
			dataKey := ".dockerconfigjson"
			valueMarker := "root-secret-value"
			if test.operation == "variables-patch" {
				capturePath = f.variablesPatchCapture
				dataKey = "ghcr_dockerconfigjson"
				valueMarker = "variables-secret-value"
			}
			var capture map[string]any
			if err := json.Unmarshal([]byte(mustRead(capturePath)), &capture); err != nil {
				t.Fatalf("decode successful Secret patch capture: %v", err)
			}
			data, ok := capture["data"].(map[string]any)
			if !ok {
				t.Fatal("successful Secret patch capture omitted data")
			}
			wantValue, ok := data[dataKey].(string)
			if !ok || wantValue == "" {
				t.Fatal("successful Secret patch capture omitted credential value")
			}
			if value := mustRead(filepath.Join(f.syncStateDir, valueMarker)); value != wantValue {
				t.Fatalf("final live Secret value = %q, want current transaction value %q", value, wantValue)
			}
			if value := mustRead(filepath.Join(f.syncStateDir, test.marker+"-live-value")); value != test.liveValue {
				t.Fatalf("live Secret value = %q, want newer transaction value %q", value, test.liveValue)
			}
			requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
		})
	}
}

func TestLeaseLossDuringSecretReadStopsBeforePatch(t *testing.T) {
	for name, test := range map[string]struct {
		environment string
		marker      string
		operation   string
		capture     func(*fixture) string
	}{
		"root Secret": {
			environment: "FAKE_SYNC_LEASE_LOST_AFTER_ROOT_SECRET_GET",
			marker:      "sync-lease-lost-after-root-secret-get",
			operation:   "root-patch",
			capture:     func(f *fixture) string { return f.patchCapture },
		},
		"variables-base Secret": {
			environment: "FAKE_SYNC_LEASE_LOST_AFTER_VARIABLES_SECRET_GET",
			marker:      "sync-lease-lost-after-variables-secret-get",
			operation:   "variables-patch",
			capture:     func(f *fixture) string { return f.variablesPatchCapture },
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			overrides := map[string]string{test.environment: "true"}
			if test.operation == "root-patch" {
				// Skip the earlier overlap-read path so the fake replaces the
				// Lease specifically inside the root Secret CAS helper.
				overrides["FAKE_TALOS_NODES_CURRENT"] = "true"
			}
			result := f.runHelper(validConfig(), nil, overrides)
			requireFailureResult(t, result)
			if !pathExists(filepath.Join(f.syncStateDir, test.marker)) {
				t.Fatal("fixture did not replace the Lease during the Secret read")
			}
			if pathExists(f.operationLog) {
				requireNoLine(t, readLines(f.operationLog), test.operation)
			}
			if pathExists(test.capture(f)) {
				t.Fatalf("%s was captured after the transaction lost its Lease", test.operation)
			}
			requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
		})
	}
}

func TestLeaseLossAfterNodeClaimStopsBeforeTalosMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_SYNC_LEASE_LOST_AFTER_FIRST_CLAIM": "true",
	})
	requireFailureResult(t, result)
	if !pathExists(filepath.Join(f.syncStateDir, "sync-lease-lost-after-claim")) {
		t.Fatal("fixture did not replace the synchronization lease after the node claim")
	}
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireNoLine(t, operations, "talos-auth:10.0.0.2")
	requireNoLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "root-patch")
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
		pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Fatal("Lease loss before Talos mutation left a newly-owned cordon behind")
	}
}

func TestLeaseLossAfterPreCordonedNodeClaimPreservesSchedulingIntent(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CORDONED_NODES":                    "prod-worker-1",
		"FAKE_SYNC_LEASE_LOST_AFTER_FIRST_CLAIM": "true",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-release-cordon-owner:prod-worker-1")
	requireNoLine(t, operations, "talos-auth:10.0.0.2")
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Fatal("Lease loss left ownership on a pre-existing cordon")
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Fatal("Lease-loss rollback removed the pre-existing cordon")
	}
}

func TestCredentialPatchFailureReleasesOwnedCordon(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
		"FAKE_TALOS_FAIL_OPERATION": "auth",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	claim := lineIndex(t, operations, "node-claim-cordon:prod-worker-1")
	auth := lineIndex(t, operations, "talos-auth:10.0.0.2")
	release := lineIndex(t, operations, "node-uncordon:prod-worker-1")
	if claim >= auth || auth >= release {
		t.Fatalf("unsafe credential-patch failure ordering: claim=%d auth=%d release=%d", claim, auth, release)
	}
	requireNoLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "root-patch")
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
		pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Fatal("credential-patch failure left the worker owned-cordoned")
	}
}

func TestCredentialPatchFailurePreservesPreExistingCordon(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CORDONED_NODES":       "prod-worker-1",
		"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
		"FAKE_TALOS_FAIL_OPERATION": "auth",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	claim := lineIndex(t, operations, "node-claim-cordon:prod-worker-1")
	auth := lineIndex(t, operations, "talos-auth:10.0.0.2")
	release := lineIndex(t, operations, "node-release-cordon-owner:prod-worker-1")
	if claim >= auth || auth >= release {
		t.Fatalf("unsafe pre-existing-cordon auth failure ordering: claim=%d auth=%d release=%d", claim, auth, release)
	}
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	requireNoLine(t, operations, "node-drain:prod-worker-1")
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Fatal("credential-patch failure left bridge ownership on the pre-existing cordon")
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Fatal("credential-patch failure removed the pre-existing cordon")
	}
}

func TestSchedulingDriftDuringCredentialPatchStopsBeforeDrain(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_EXTERNAL_UNCORDON_AFTER_AUTH_NODE": "prod-worker-1",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "scheduling safety state changed before drain")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireLine(t, operations, "talos-auth:10.0.0.2")
	requireLine(t, operations, "operator-uncordon-after-auth:prod-worker-1")
	requireNoLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "talos-reboot:10.0.0.2")
	requireNoLine(t, operations, "root-patch")
}

func TestSchedulingDriftDuringImageMutationStopsAtNextGuard(t *testing.T) {
	for name, test := range map[string]struct {
		environment string
		marker      string
		guard       string
		pullRan     bool
	}{
		"after remove": {
			environment: "FAKE_EXTERNAL_UNCORDON_AFTER_REMOVE_NODE",
			marker:      "operator-uncordon-after-remove:prod-worker-1",
			guard:       "before image pull",
		},
		"after pull": {
			environment: "FAKE_EXTERNAL_UNCORDON_AFTER_PULL_NODE",
			marker:      "operator-uncordon-after-pull:prod-worker-1",
			guard:       "before runtime pull proof",
			pullRan:     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{
				test.environment: "prod-worker-1",
			})
			requireFailureResult(t, result)
			requireContains(t, result.stdout+result.stderr, test.guard)
			operations := readLines(f.operationLog)
			requireLine(t, operations, "talos-remove:10.0.0.2:"+ksailTargetImage)
			requireLine(t, operations, test.marker)
			if test.pullRan {
				requireLine(t, operations, "talos-pull:10.0.0.2:"+ksailTargetImage)
			} else {
				requireNoLine(t, operations, "talos-pull:10.0.0.2:"+ksailTargetImage)
			}
			requireNoLine(t, operations, "talos-revision:10.0.0.2")
			requireNoLine(t, operations, "node-uncordon:prod-worker-1")
			requireNoLine(t, operations, "root-patch")
		})
	}
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

func TestTransientLifecycleTaintsClearBeforeImageMutation(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TRANSIENT_LIFECYCLE_TAINT_AFTER_READY_NODE": "prod-worker-1",
	})
	requireSuccessResult(t, result)
	if reads := parseInt(mustRead(filepath.Join(f.syncStateDir, "post-ready-node-read-count-prod-worker-1")), 0); reads < 2 {
		t.Errorf("post-Ready node reads = %d, want at least 2", reads)
	}
	operations := readLines(f.operationLog)
	ready := lineIndex(t, operations, "node-ready:prod-worker-1")
	remove := lineIndex(t, operations, "talos-remove:10.0.0.2:"+ksailTargetImage)
	if ready >= remove {
		t.Errorf("Ready index %d is not before image removal index %d", ready, remove)
	}
	requireLine(t, operations, "node-uncordon:prod-worker-1")
	requireLine(t, operations, "talos-revision:10.0.0.2")
	requireLine(t, operations, "root-patch")
}

func TestPersistentLifecycleTaintsKeepNodeCordoned(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_PERSISTENT_LIFECYCLE_TAINT_AFTER_READY_NODE": "prod-worker-1",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "lifecycle taints to clear")
	if reads := mustRead(filepath.Join(f.syncStateDir, "post-ready-node-read-count-prod-worker-1")); reads != "2" {
		t.Errorf("post-Ready node reads = %q, want bounded 2", reads)
	}
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-ready:prod-worker-1")
	for _, unexpected := range []string{
		"talos-remove:10.0.0.2:" + ksailTargetImage,
		"node-uncordon:prod-worker-1",
		"talos-revision:10.0.0.2",
		"root-patch",
	} {
		requireNoLine(t, operations, unexpected)
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
		!pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Error("persistent lifecycle taints did not preserve the owned cordon")
	}
}

func TestReadyFalseWithoutLifecycleTaintKeepsNodeCordoned(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_NOT_READY_WITHOUT_LIFECYCLE_TAINT_NODE": "prod-worker-1",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "remain Ready and for post-reboot lifecycle taints to clear")
	if reads := mustRead(filepath.Join(f.syncStateDir, "post-ready-node-read-count-prod-worker-1")); reads != "2" {
		t.Errorf("post-Ready node reads = %q, want bounded 2", reads)
	}
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-ready:prod-worker-1")
	for _, unexpected := range []string{
		"talos-remove:10.0.0.2:" + ksailTargetImage,
		"node-uncordon:prod-worker-1",
		"talos-revision:10.0.0.2",
		"root-patch",
	} {
		requireNoLine(t, operations, unexpected)
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) ||
		!pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
		t.Error("Ready=False state did not preserve the owned cordon")
	}
}

func TestReplacementAfterUncordonBlocksConvergence(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_REPLACED_AFTER_UNCORDON_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "identity changed")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-uncordon:prod-worker-1")
	requireLine(t, operations, "talos-revision:10.0.0.2")
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

func TestUncordonFailureKeepsRevisionMarkerAndOwnerFailClosed(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_UNCORDON_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireLine(t, operations, "talos-revision:10.0.0.2")
	for _, unexpected := range []string{"node-uncordon:prod-worker-1", "root-patch"} {
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
		"node-claim-cordon:prod-worker-1",
		"talos-auth:10.0.0.2",
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
