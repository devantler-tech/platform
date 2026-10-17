package refreshfluxghcrauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCompletedTransactionClearsNodePhaseMarkers(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"ordinary":              {},
		"operator cordon":       {"FAKE_CORDONED_NODES": "prod-worker-1"},
		"lost release response": {"FAKE_NODE_RELEASE_RESPONSE_LOST_NODE": "prod-worker-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			result := f.runHelper(validConfig(), nil, env)
			requireSuccessResult(t, result)
			for _, node := range []string{"prod-worker-1", "prod-control-plane-1"} {
				for _, marker := range []string{"cordon-owner-", "cordon-phase-", "cordon-recovery-"} {
					if pathExists(filepath.Join(f.syncStateDir, marker+node)) {
						t.Errorf("completed transaction retained %s%s", marker, node)
					}
				}
			}
			if name == "operator cordon" && !pathExists(filepath.Join(f.syncStateDir, "cordoned-prod-worker-1")) {
				t.Error("operator cordon was removed")
			}
		})
	}
}

func TestReleaseReadbackRequiresPhaseRemoval(t *testing.T) {
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	body := functionBody(t, script, "node_schedulability_release_is_complete") + "\n}\n"
	for _, tc := range []struct {
		name        string
		annotations map[string]any
		cordoned    bool
		wasCordoned int
		want        bool
	}{
		{name: "complete", annotations: map[string]any{}, want: true},
		{name: "claimed remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-phase": "claimed"}},
		{name: "mutating remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-phase": "mutating"}},
		{name: "unknown phase remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-phase": "unknown"}},
		{name: "empty phase remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-phase": ""}},
		{name: "owner remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-owner": "other"}},
		{name: "recovery remains", annotations: map[string]any{"platform.devantler.tech/ghcr-auth-drain-recovery": "journal"}},
		{name: "operator cordon retained", annotations: map[string]any{}, cordoned: true, wasCordoned: 1, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			node := map[string]any{"metadata": map[string]any{"uid": "fixture-uid", "annotations": tc.annotations}, "spec": map[string]any{"unschedulable": tc.cordoned}}
			data, err := json.Marshal(node)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "node.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			preamble := `CORDON_OWNER_ANNOTATION=platform.devantler.tech/ghcr-auth-drain-owner
CORDON_RECOVERY_ANNOTATION=platform.devantler.tech/ghcr-auth-drain-recovery
CORDON_PHASE_ANNOTATION=platform.devantler.tech/ghcr-auth-drain-phase
SCALE_DOWN_GUARD_OWNER_ANNOTATION=platform.devantler.tech/ghcr-auth-scale-down-owner
AUTOSCALER_SCALE_DOWN_DISABLED_ANNOTATION=cluster-autoscaler.kubernetes.io/scale-down-disabled
`
			result := runCaptured(t, dir, nil, "bash", "-c", preamble+body+`node_schedulability_release_is_complete "$1" fixture-uid "$2" 0`, "fixture", path, strconv.Itoa(tc.wasCordoned))
			if (result.exitCode == 0) != tc.want {
				t.Fatalf("exit=%d, complete=%v\n%s", result.exitCode, tc.want, result.stderr)
			}
		})
	}
}

func TestFailedPhaseAdvanceReleasesClaimedMarker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result := f.runHelper(validConfig(), nil, map[string]string{"FAKE_FENCE_PHASE_FAIL_NODE": "prod-worker-1"})
	requireFailureResult(t, result)
	requireNoLine(t, readLines(f.operationLog), "talos-auth:10.0.0.2")
	for _, marker := range []string{"cordon-owner-", "cordon-phase-"} {
		if pathExists(filepath.Join(f.syncStateDir, marker+"prod-worker-1")) {
			t.Errorf("failed phase advance retained %s", marker)
		}
	}
}

func TestFakeNodeReleaseHonorsOnlySubmittedPhaseOperations(t *testing.T) {
	const phasePath = "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-phase"
	for _, tc := range []struct {
		name, phase, testPhase string
		remove, success        bool
	}{
		{name: "omitted removal preserves phase", phase: "mutating", success: true},
		{name: "claimed release", phase: "claimed", testPhase: "claimed", remove: true, success: true},
		{name: "mutating release", phase: "mutating", testPhase: "mutating", remove: true, success: true},
		{name: "legacy absent phase", success: true},
		{name: "changed phase rejects atomically", phase: "mutating", testPhase: "claimed", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("FAKE_SYNC_STATE_DIR", dir)
			t.Setenv("OPERATION_LOG", filepath.Join(dir, "operations.log"))
			const node = "prod-worker-1"
			setMarkerContent("cordon-owner-"+node, "fixture-owner")
			if tc.phase != "" {
				setMarkerContent("cordon-phase-"+node, tc.phase)
			}
			touchMarker("cordoned-" + node)
			patch := []jsonPatchOperation{
				{Operation: "test", Path: "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner", Value: "fixture-owner"},
				{Operation: "test", Path: "/metadata/uid", Value: fakeExpectedNodeUID(node)},
				{Operation: "test", Path: "/metadata/resourceVersion", Value: "10"},
			}
			if tc.remove {
				patch = append(patch, jsonPatchOperation{Operation: "test", Path: phasePath, Value: tc.testPhase}, jsonPatchOperation{Operation: "remove", Path: phasePath})
			}
			patch = append(patch, jsonPatchOperation{Operation: "remove", Path: "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner"}, jsonPatchOperation{Operation: "add", Path: "/spec/unschedulable", Value: false})
			data, err := json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "patch.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			exit := fakeKubectlPatchNode([]string{"patch", "node", node}, path)
			if (exit == 0) != tc.success {
				t.Fatalf("exit=%d want success=%v", exit, tc.success)
			}
			expected := tc.phase
			if tc.remove && tc.success {
				expected = ""
			}
			if got := markerContent("cordon-phase-" + node); got != expected {
				t.Fatalf("phase=%q want %q", got, expected)
			}
			if !tc.success && (markerContent("cordon-owner-"+node) != "fixture-owner" || markerContent("resource-version-"+node) != "" || !markerExists("cordoned-"+node)) {
				t.Fatal("rejected patch partially changed state")
			}
		})
	}
}
