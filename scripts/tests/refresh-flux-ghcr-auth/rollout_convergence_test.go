package refreshfluxghcrauth

import (
	"strings"
	"testing"
)

func TestSecondFanoutVerificationBlocksRootCutover(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_CONSUMER_MISMATCH_ON_SECOND_PASS_NAMESPACE": "wedding-app"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "did not materialise")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-revision:10.0.0.1")
	count := 0
	for _, operation := range operations {
		if operation == "variables-patch" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("variables patch count = %d, want 2", count)
	}
	requireNoLine(t, operations, "root-patch")
}

func TestMissingCachedImageStillPullsAndRecordsRevision(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_TALOS_IMAGE_ABSENT_NODE": "10.0.0.2"})
	requireSuccessResult(t, result)
	operations := readLines(f.talosLog)
	requireLine(t, operations, "talos-remove:10.0.0.2:"+ksailTargetImage)
	requireLine(t, operations, "talos-pull:10.0.0.2:"+ksailTargetImage)
	requireLine(t, operations, "talos-revision:10.0.0.2")
	if !pathExists(f.patchCapture) {
		t.Error("root patch missing after successful pull proof")
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func TestCurrentTalosNodesSkipTalosAPI(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"})
	requireSuccessResult(t, result)
	if pathExists(f.talosLog) {
		t.Error("current nodes unexpectedly invoked Talos")
	}
	if !pathExists(f.patchCapture) {
		t.Error("root patch missing")
	}
}

func TestMatchingRevisionRevalidatesChangedDeclaredImage(t *testing.T) {
	f := newFixture(t)
	previousImage := "ghcr.io/devantler-tech/ksail:v7.166.0"
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":  "true",
		"FAKE_TALOS_VERIFIED_IMAGE": previousImage,
	})
	requireSuccessResult(t, result)
	if !pathExists(f.talosLog) {
		t.Fatal("matching revision incorrectly skipped changed-image proof")
	}
	operations := readLines(f.talosLog)
	requireLinesEqual(t, operations, []string{
		"talos-remove:10.0.0.2:" + ksailTargetImage,
		"talos-pull:10.0.0.2:" + ksailTargetImage,
		"talos-revision:10.0.0.2",
		"talos-remove:10.0.0.1:" + ksailTargetImage,
		"talos-pull:10.0.0.1:" + ksailTargetImage,
		"talos-revision:10.0.0.1",
	})
	operationLog := mustRead(f.operationLog)
	requireNotContains(t, operationLog, "node-drain:")
	requireNotContains(t, operationLog, "talos-reboot:")
	requireNotContains(t, strings.Join(operations, "\n"), previousImage)
}

func TestFailedImageOnlyPullKeepsNodeCordoned(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":  "true",
		"FAKE_TALOS_VERIFIED_IMAGE": "ghcr.io/devantler-tech/ksail:v7.166.0",
		"FAKE_TALOS_FAIL_NODE":      "10.0.0.2",
		"FAKE_TALOS_FAIL_OPERATION": "pull",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-claim-cordon:prod-worker-1")
	for _, unexpected := range []string{"node-drain:prod-worker-1", "node-uncordon:prod-worker-1", "talos-reboot:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
}

func TestNodeAddedMidRollIsProcessedBeforeRootCutover(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_APPEARS_AFTER_ROLL": "prod-worker-2"})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	for _, expected := range []string{"talos-auth:10.0.0.5", "node-drain:prod-worker-2", "talos-reboot:10.0.0.5", "talos-revision:10.0.0.5"} {
		requireLine(t, operations, expected)
	}
	if lineIndex(t, operations, "talos-revision:10.0.0.5") >= lineIndex(t, operations, "root-patch") {
		t.Error("root cutover preceded late-node proof")
	}
}

func TestNodeAddedDuringSecondFanoutIsProcessedBeforeCutover(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_APPEARS_DURING_SECOND_FANOUT": "prod-worker-2"})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	variables := lineIndices(operations, "variables-patch")
	if len(variables) < 2 {
		t.Fatalf("variables fanout passes = %d, want at least 2", len(variables))
	}
	lateRevision := lineIndex(t, operations, "talos-revision:10.0.0.5")
	rootCutover := lineIndex(t, operations, "root-patch")
	if variables[1] >= lateRevision || lateRevision >= rootCutover {
		t.Errorf("unsafe late-node ordering: fanout=%d revision=%d root=%d", variables[1], lateRevision, rootCutover)
	}
}

func TestLateNodeRollReprovesFanoutBeforeRootCutover(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_NODE_APPEARS_DURING_SECOND_FANOUT":          "prod-worker-2",
		"FAKE_CONSUMER_REVERT_DURING_LATE_NODE_NAMESPACE": "wedding-app",
	})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	fanoutStarts := lineIndices(operations, "variables-patch")
	if len(fanoutStarts) != 3 {
		t.Fatalf("fanout pass count = %d, want 3", len(fanoutStarts))
	}
	consumerRevert := lineIndex(t, operations, "consumer-revert:wedding-app")
	rootCutover := lineIndex(t, operations, "root-patch")
	if consumerRevert >= fanoutStarts[2] || fanoutStarts[2] >= rootCutover {
		t.Errorf("unsafe re-proof ordering: revert=%d third-fanout=%d root=%d", consumerRevert, fanoutStarts[2], rootCutover)
	}
}

func lineIndices(lines []string, target string) []int {
	var result []int
	for index, line := range lines {
		if line == target {
			result = append(result, index)
		}
	}
	return result
}

func TestRevokedPreviousCredentialBlocksFirstDrain(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_REVOKE_CURRENT_ROOT_TOKEN": "true"})
	requireFailureResult(t, result)
	output := result.stdout + result.stderr
	requireContains(t, output, "current root GHCR credential")
	operations := readLines(f.operationLog)
	requireNotContains(t, strings.Join(operations, "\n"), "node-drain:")
	requireNotContains(t, strings.Join(operations, "\n"), "talos-reboot:")
	requireNoLine(t, operations, "root-patch")
	requireNotContains(t, output, "previous-runtime-token")
}

func TestValidRootTokenDoesNotSubstituteForPeerRuntimeProof(t *testing.T) {
	f := newFixture(t)
	ready := []any{map[string]any{"type": "Ready", "status": "True"}}
	inventory := map[string]any{"items": []any{
		nodeFixture("prod-worker-1", "prod-worker-1-uid", "10.0.0.2", false, ready, nil),
		nodeFixture("prod-control-plane-1", "prod-control-plane-1-uid", "10.0.0.1", true, ready, nil),
		nodeFixture("prod-control-plane-2", "prod-control-plane-2-uid", "10.0.0.3", true, ready, nil),
		nodeFixture("prod-control-plane-3", "prod-control-plane-3-uid", "10.0.0.5", true, ready, nil),
	}}
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_NODE_JSON":               encodeJSON(inventory),
		"FAKE_RUNTIME_PULL_FAIL_NODES": "prod-control-plane-1 prod-control-plane-2 prod-control-plane-3",
	})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "running containerd")
	operations := readLines(f.operationLog)
	requireNotContains(t, strings.Join(operations, "\n"), "node-drain:")
	requireNoLine(t, operations, "root-patch")
}

func TestRuntimeProbeRejectsInjectedImagePullSecret(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_RUNTIME_PROBE_INJECT_PULL_SECRET_NODES": "prod-control-plane-2"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "imagePullSecret")
	operations := readLines(f.operationLog)
	requireNotContains(t, strings.Join(operations, "\n"), "node-drain:")
	requireNoLine(t, operations, "root-patch")
}

func TestEachPrivateRuntimePackageACLMustPass(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/devantler-tech/wedding-app:latest",
		"ghcr.io/devantler-tech/ascoachingogvaner:latest",
	} {
		t.Run(image, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_RUNTIME_PULL_FAIL_IMAGES": image})
			requireFailureResult(t, result)
			requireContains(t, result.stdout+result.stderr, image)
			operations := readLines(f.operationLog)
			requireNotContains(t, strings.Join(operations, "\n"), "node-drain:")
			requireNoLine(t, operations, "root-patch")
		})
	}
}

func TestDRWithoutFanoutDoesNotDrainNodes(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), []string{"--allow-incomplete-fanout"}, map[string]string{"FAKE_VARIABLES_BASE_ABSENT": "true"})
	requireSuccessResult(t, result)
	if pathExists(f.talosLog) {
		t.Error("DR without fanout invoked Talos")
	}
	if !pathExists(f.patchCapture) {
		t.Error("DR root repair patch missing")
	}
}

func TestInvalidNodeInventoryFailsClosed(t *testing.T) {
	invalidInventories := []any{
		map[string]any{"items": []any{}},
		map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "one", "uid": "uid-one"},
			"status":   map[string]any{"addresses": []any{}},
		}}},
		map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "one", "uid": "uid-one"},
			"status": map[string]any{"addresses": []any{
				map[string]any{"type": "InternalIP", "address": "10.0.0.1"},
				map[string]any{"type": "InternalIP", "address": "10.0.0.2"},
			}},
		}}},
		map[string]any{"items": []any{
			nodeFixture("one", "uid-one", "10.0.0.1", false, nil, nil),
			nodeFixture("two", "uid-two", "10.0.0.1", false, nil, nil),
		}},
		map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "one"},
			"status": map[string]any{"addresses": []any{
				map[string]any{"type": "InternalIP", "address": "10.0.0.1"},
			}},
		}}},
		map[string]any{"items": []any{
			nodeFixture("one", "duplicate", "10.0.0.1", false, nil, nil),
			nodeFixture("two", "duplicate", "10.0.0.2", false, nil, nil),
		}},
	}
	for index, inventory := range invalidInventories {
		t.Run(string(rune('A'+index)), func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_JSON": encodeJSON(inventory)})
			requireFailureResult(t, result)
			if pathExists(f.talosLog) {
				t.Error("invalid inventory invoked Talos")
			}
			if pathExists(f.patchCapture) {
				t.Error("invalid inventory changed root auth")
			}
		})
	}
}

func TestNodeDiscoveryFailureAfterSafeFanoutKeepsRootUnchanged(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_DISCOVERY_FAIL": "true"})
	requireFailureResult(t, result)
	if pathExists(f.talosLog) {
		t.Error("failed discovery invoked Talos")
	}
	if !pathExists(f.variablesPatchCapture) || !pathExists(f.fanoutLog) {
		t.Error("failed discovery did not occur after safe fanout")
	}
	if pathExists(f.patchCapture) {
		t.Error("failed discovery changed root auth")
	}
}
