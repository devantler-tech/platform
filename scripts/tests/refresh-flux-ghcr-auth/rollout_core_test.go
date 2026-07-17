package refreshfluxghcrauth

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func requireSuccessResult(t *testing.T, result commandResult) {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("command exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", result.exitCode, result.stdout, result.stderr)
	}
}

func requireFailureResult(t *testing.T, result commandResult) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatalf("command unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", result.stdout, result.stderr)
	}
}

func requireLinesEqual(t *testing.T, actual, expected []string) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("lines differ\nactual: %#v\nexpected: %#v", actual, expected)
	}
}

func lineIndex(t *testing.T, lines []string, target string) int {
	t.Helper()
	for index, line := range lines {
		if line == target {
			return index
		}
	}
	t.Fatalf("line %q not found in %#v", target, lines)
	return -1
}

func requireLine(t *testing.T, lines []string, target string) {
	t.Helper()
	_ = lineIndex(t, lines, target)
}

func requireNoLine(t *testing.T, lines []string, target string) {
	t.Helper()
	for _, line := range lines {
		if line == target {
			t.Errorf("unexpected line %q in %#v", target, lines)
		}
	}
}

func TestRefreshesRootAndFanoutWithoutLeakingPlaintext(t *testing.T) {
	f := newFixture(t)
	config := validConfig()
	result := f.runHelper(config, nil, nil)
	requireSuccessResult(t, result)

	var patch map[string]any
	if err := json.Unmarshal([]byte(mustRead(f.patchCapture)), &patch); err != nil {
		t.Fatalf("decode root patch: %v", err)
	}
	encoded := patch["data"].(map[string]any)[".dockerconfigjson"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode root credentials: %v", err)
	}
	var decodedConfig any
	if err := json.Unmarshal(decoded, &decodedConfig); err != nil {
		t.Fatalf("parse root credentials: %v", err)
	}
	if !reflect.DeepEqual(decodedConfig, config) {
		t.Errorf("root credentials = %#v, want %#v", decodedConfig, config)
	}

	var variablesPatch map[string]any
	if err := json.Unmarshal([]byte(mustRead(f.variablesPatchCapture)), &variablesPatch); err != nil {
		t.Fatalf("decode variables patch: %v", err)
	}
	variablesEncoded := variablesPatch["data"].(map[string]any)["ghcr_dockerconfigjson"].(string)
	variablesDecoded, err := base64.StdEncoding.DecodeString(variablesEncoded)
	if err != nil {
		t.Fatalf("decode variables credentials: %v", err)
	}
	var decodedVariables any
	if err := json.Unmarshal(variablesDecoded, &decodedVariables); err != nil {
		t.Fatalf("parse variables credentials: %v", err)
	}
	if !reflect.DeepEqual(decodedVariables, config) {
		t.Errorf("variables credentials = %#v, want %#v", decodedVariables, config)
	}
	temporaryConfig := strings.TrimSpace(mustRead(f.outputPathLog))
	if pathExists(temporaryConfig) {
		t.Errorf("temporary decrypted config still exists: %s", temporaryConfig)
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")

	requiredRegistryReads := []string{
		"devantler-tech/platform/manifests:latest",
		"devantler-tech/wedding-app/manifests:latest",
		"devantler-tech/ascoachingogvaner/manifests:latest",
		"devantler-tech/wedding-app:latest",
		"devantler-tech/ascoachingogvaner:latest",
		"devantler-tech/ksail:v" + ksailOperatorVersion,
		"devantler-tech/provider-upjet-unifi:v0.1.0",
	}
	requireLinesEqual(t, readLines(f.registryReadLog), append(append([]string{}, requiredRegistryReads...), requiredRegistryReads...))
	requireLinesEqual(t, readLines(f.fanoutLog), []string{
		"pushsecret/flux-system/seed-ghcr",
		"externalsecret/wedding-app/ghcr-auth",
		"externalsecret/ascoachingogvaner/ghcr-auth",
		"externalsecret/kyverno/ghcr-auth",
		"pushsecret/flux-system/seed-ghcr",
		"externalsecret/wedding-app/ghcr-auth",
		"externalsecret/ascoachingogvaner/ghcr-auth",
		"externalsecret/kyverno/ghcr-auth",
	})
}

func TestStagesKubernetesConsumersBeforeTalosDrains(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	target := ksailTargetImage
	requireLinesEqual(t, readLines(f.talosLog), []string{
		"talos-auth:10.0.0.2",
		"talos-reboot:10.0.0.2",
		"talos-remove:10.0.0.2:" + target,
		"talos-pull:10.0.0.2:" + target,
		"talos-revision:10.0.0.2",
		"talos-auth:10.0.0.1",
		"talos-reboot:10.0.0.1",
		"talos-remove:10.0.0.1:" + target,
		"talos-pull:10.0.0.1:" + target,
		"talos-revision:10.0.0.1",
	})
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
		"talos-remove:10.0.0.2:" + target,
		"talos-pull:10.0.0.2:" + target,
		"node-uncordon:prod-worker-1",
		"talos-revision:10.0.0.2",
		"talos-auth:10.0.0.1",
		"node-claim-cordon:prod-control-plane-1",
		"node-drain:prod-control-plane-1",
		"talos-reboot:10.0.0.1",
		"node-ready:prod-control-plane-1",
		"talos-remove:10.0.0.1:" + target,
		"talos-pull:10.0.0.1:" + target,
		"node-uncordon:prod-control-plane-1",
		"talos-revision:10.0.0.1",
		"variables-patch",
		"fanout:pushsecret/flux-system/seed-ghcr",
		"fanout:externalsecret/wedding-app/ghcr-auth",
		"fanout:externalsecret/ascoachingogvaner/ghcr-auth",
		"fanout:externalsecret/kyverno/ghcr-auth",
		"root-patch",
	})
	temporaryPatch := strings.TrimSpace(mustRead(f.talosPatchPathLog))
	if pathExists(temporaryPatch) {
		t.Errorf("temporary Talos patch still exists: %s", temporaryPatch)
	}
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func TestUnhealthyControlPlaneBlocksTheControlPlaneReboot(t *testing.T) {
	f := newFixture(t)
	ready := []any{map[string]any{"type": "Ready", "status": "True"}}
	inventory := map[string]any{"items": []any{
		nodeFixture("prod-worker-1", "prod-worker-1-uid", "10.0.0.2", false, ready, nil),
		nodeFixture("prod-control-plane-1", "prod-control-plane-1-uid", "10.0.0.1", true, ready, nil),
		nodeFixture("prod-control-plane-2", "prod-control-plane-2-uid", "10.0.0.3", true,
			[]any{map[string]any{"type": "Ready", "status": "False"}},
			map[string]any{
				"platform.devantler.tech/ghcr-pull-verified-revision-v2": f.expectedRevision(),
				"platform.devantler.tech/ghcr-pull-verified-image-v2":    ksailTargetImage,
			}),
		nodeFixture("prod-control-plane-3", "prod-control-plane-3-uid", "10.0.0.4", true, ready,
			map[string]any{
				"platform.devantler.tech/ghcr-pull-verified-revision-v2": f.expectedRevision(),
				"platform.devantler.tech/ghcr-pull-verified-image-v2":    ksailTargetImage,
			}),
	}}
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_NODE_JSON": encodeJSON(inventory)})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "risks quorum")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-reboot:10.0.0.2")
	requireNoLine(t, operations, "talos-reboot:10.0.0.1")
	requireNoLine(t, operations, "root-patch")
	requireNotContains(t, result.stdout+result.stderr, "fixture-secret-token")
}

func nodeFixture(name, uid, internalIP string, controlPlane bool, conditions []any, annotations map[string]any) map[string]any {
	labels := map[string]any{}
	if controlPlane {
		labels["node-role.kubernetes.io/control-plane"] = ""
	}
	metadata := map[string]any{"name": name, "uid": uid, "labels": labels}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	return map[string]any{
		"metadata": metadata,
		"status": map[string]any{
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": internalIP}},
			"conditions": conditions,
		},
	}
}

func TestControlPlaneQuorumIsRecheckedAfterTheDrain(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_ETCD_STATUS_FAIL_AFTER_DRAIN_NODE": "10.0.0.3"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "risks quorum")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-drain:prod-control-plane-1")
	requireLine(t, operations, "node-uncordon:prod-control-plane-1")
	requireNoLine(t, operations, "talos-reboot:10.0.0.1")
	requireNoLine(t, operations, "talos-revision:10.0.0.1")
	requireNoLine(t, operations, "root-patch")
}

func TestUnsafeEtcdMemberStatusBlocksControlPlaneReboot(t *testing.T) {
	for _, variable := range []string{"FAKE_ETCD_LEARNER_NODE", "FAKE_ETCD_STATUS_ERROR_NODE"} {
		t.Run(variable, func(t *testing.T) {
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, map[string]string{variable: "10.0.0.3"})
			requireFailureResult(t, result)
			operations := readLines(f.operationLog)
			requireLine(t, operations, "talos-reboot:10.0.0.2")
			requireNoLine(t, operations, "talos-reboot:10.0.0.1")
			requireNoLine(t, operations, "root-patch")
		})
	}
}

func TestCompactHealthyEtcdStatusPermitsControlPlaneReboot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_ETCD_COMPACT_STATUS_NODE": "10.0.0.3"})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "talos-reboot:10.0.0.1")
	requireLine(t, operations, "root-patch")
}

func TestPreExistingCordonSurvivesTheAuthReboot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_CORDONED_NODES": "prod-worker-1"})
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	requireLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	requireLine(t, operations, "node-claim-cordon:prod-control-plane-1")
	requireLine(t, operations, "node-uncordon:prod-control-plane-1")
}

func TestSchedulableNodeIsUncordonedAfterTheAuthReboot(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	claim := lineIndex(t, operations, "node-claim-cordon:prod-worker-1")
	drain := lineIndex(t, operations, "node-drain:prod-worker-1")
	pull := lineIndex(t, operations, "talos-pull:10.0.0.2:"+ksailTargetImage)
	uncordon := lineIndex(t, operations, "node-uncordon:prod-worker-1")
	revision := lineIndex(t, operations, "talos-revision:10.0.0.2")
	if claim >= drain || drain >= pull || pull >= uncordon || uncordon >= revision {
		t.Errorf("unsafe worker ordering: claim=%d drain=%d pull=%d uncordon=%d revision=%d", claim, drain, pull, uncordon, revision)
	}
	if actual := mustRead(filepath.Join(f.syncStateDir, "resource-version-prod-worker-1")); actual != "12" {
		t.Errorf("resource version = %q, want 12", actual)
	}
}

func TestSchedulableNodeIsClaimedAndCordonedAtomically(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, nil)
	requireSuccessResult(t, result)
	operations := readLines(f.operationLog)
	claim := lineIndex(t, operations, "node-claim-cordon:prod-worker-1")
	drain := lineIndex(t, operations, "node-drain:prod-worker-1")
	if claim >= drain {
		t.Errorf("claim index %d is not before drain index %d", claim, drain)
	}
	requireNoLine(t, operations, "node-claim:prod-worker-1")
}

func TestConcurrentCordonBeforeAtomicClaimStopsTheRoll(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_CORDON_BEFORE_CLAIM_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireContains(t, result.stdout+result.stderr, "Could not atomically claim and cordon")
	operations := readLines(f.operationLog)
	requireLine(t, operations, "operator-cordon:prod-worker-1")
	for _, unexpected := range []string{"node-claim-cordon:prod-worker-1", "node-drain:prod-worker-1", "talos-reboot:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Error("failed atomic claim left an owner marker")
	}
}

func TestPDBBlockedDrainRestoresOriginalSchedulability(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_DRAIN_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	output := result.stdout + result.stderr
	requireContains(t, output, "drain: cannot evict pod backstage-db-4: would violate PodDisruptionBudget backstage-db-primary")
	operations := readLines(f.operationLog)
	for _, expected := range []string{"node-claim-cordon:prod-worker-1", "node-drain:prod-worker-1", "node-uncordon:prod-worker-1"} {
		requireLine(t, operations, expected)
	}
	for _, unexpected := range []string{"talos-reboot:10.0.0.2", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
	requireNotContains(t, output, "fixture-secret-token")
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Error("PDB failure left an owner marker")
	}
}

func TestPDBBlockedDrainPreservesPreExistingCordon(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{
		"FAKE_CORDONED_NODES":  "prod-worker-1",
		"FAKE_DRAIN_FAIL_NODE": "prod-worker-1",
	})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	requireNoLine(t, operations, "node-claim-cordon:prod-worker-1")
	requireLine(t, operations, "node-drain:prod-worker-1")
	requireNoLine(t, operations, "node-uncordon:prod-worker-1")
	requireNoLine(t, operations, "talos-reboot:10.0.0.2")
}

func TestDrainAPIFailureReleasesTheAtomicClaim(t *testing.T) {
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_DRAIN_API_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	operations := readLines(f.operationLog)
	for _, expected := range []string{"node-claim-cordon:prod-worker-1", "node-drain:prod-worker-1", "node-uncordon:prod-worker-1"} {
		requireLine(t, operations, expected)
	}
	for _, unexpected := range []string{"talos-reboot:10.0.0.2", "talos-revision:10.0.0.2", "root-patch"} {
		requireNoLine(t, operations, unexpected)
	}
	if pathExists(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")) {
		t.Error("drain API failure left an owner marker")
	}
}
