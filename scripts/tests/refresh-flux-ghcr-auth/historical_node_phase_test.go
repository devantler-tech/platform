package refreshfluxghcrauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalNodePhasePlanRequiresCurrentRuntimeProof(t *testing.T) {
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth-safety.sh")
	body := functionBody(t, script, "completed_node_phase_cleanup_patch") + "\n}\n"
	for _, tc := range []struct {
		name    string
		change  func(map[string]any, map[string]any)
		want    bool
		noPatch bool
	}{
		{name: "mutating", want: true},
		{name: "claimed", want: true, change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-drain-phase"] = "claimed" }},
		{name: "absent", want: true, noPatch: true, change: func(n, a map[string]any) { delete(a, "platform.devantler.tech/ghcr-auth-drain-phase") }},
		{name: "unknown phase", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-drain-phase"] = "unknown" }},
		{name: "empty phase", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-drain-phase"] = "" }},
		{name: "active owner", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-drain-owner"] = "other" }},
		{name: "recovery", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-drain-recovery"] = "journal" }},
		{name: "scale guard owner", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-auth-scale-down-owner"] = "other" }},
		{name: "operator cordon", change: func(n, a map[string]any) { n["spec"].(map[string]any)["unschedulable"] = true }},
		{name: "missing revision", change: func(n, a map[string]any) { delete(a, "platform.devantler.tech/ghcr-pull-verified-revision-v2") }},
		{name: "stale revision", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-pull-verified-revision-v2"] = "old" }},
		{name: "stale image", change: func(n, a map[string]any) { a["platform.devantler.tech/ghcr-pull-verified-image-v2"] = "old" }},
		{name: "changed identity", change: func(n, a map[string]any) { n["metadata"].(map[string]any)["uid"] = "replacement" }},
		{name: "missing identity", change: func(n, a map[string]any) { delete(n["metadata"].(map[string]any), "uid") }},
		{name: "missing resource version", change: func(n, a map[string]any) { delete(n["metadata"].(map[string]any), "resourceVersion") }},
		{name: "deleting", change: func(n, a map[string]any) {
			n["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-04T00:00:00Z"
		}},
		{name: "unready", change: func(n, a map[string]any) {
			n["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := map[string]any{
				"platform.devantler.tech/ghcr-auth-drain-phase":          "mutating",
				"platform.devantler.tech/ghcr-pull-verified-revision-v2": "revision",
				"platform.devantler.tech/ghcr-pull-verified-image-v2":    "image",
				"platform.devantler.tech/ghcr-pull-verified-node-uid-v2": "fixture-uid",
			}
			n := map[string]any{"metadata": map[string]any{"uid": "fixture-uid", "resourceVersion": "10", "annotations": a}, "spec": map[string]any{"unschedulable": false}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
			if tc.change != nil {
				tc.change(n, a)
			}
			data, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "node.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			result := runCaptured(t, dir, nil, "bash", "-c", body+`completed_node_phase_cleanup_patch "$1" revision image`, "fixture", path)
			if (result.exitCode == 0) != tc.want {
				t.Fatalf("exit=%d want=%v\n%s", result.exitCode, tc.want, result.stderr)
			}
			if !tc.want {
				return
			}
			var patch []jsonPatchOperation
			if err := json.Unmarshal([]byte(result.stdout), &patch); err != nil {
				t.Fatal(err)
			}
			if tc.noPatch {
				if len(patch) != 0 {
					t.Fatal("absent phase caused a write")
				}
				return
			}
			if len(patch) != 4 || !hasPatchOperation(patch, "test", "/metadata/uid", "fixture-uid") || !hasPatchOperation(patch, "test", "/metadata/resourceVersion", "10") || !hasPatchOperation(patch, "test", "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-phase", a["platform.devantler.tech/ghcr-auth-drain-phase"]) || !hasPatchPath(patch, "remove", "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-phase") {
				t.Fatalf("unbounded cleanup patch: %s", result.stdout)
			}
		})
	}
}

func TestHistoricalNodePhaseIsClearedWithoutNodeSynchronization(t *testing.T) {
	for _, phase := range []string{"claimed", "mutating"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if err := os.WriteFile(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1"), []byte(phase), 0600); err != nil {
				t.Fatal(err)
			}
			result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"})
			requireSuccessResult(t, result)
			if pathExists(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1")) {
				t.Fatal("no-target transaction retained historical phase")
			}
			if pathExists(f.talosLog) {
				t.Fatal("historical metadata cleanup performed Talos work")
			}
			for _, op := range []string{"drain:prod-worker-1", "reboot:10.0.0.2", "talos-auth:10.0.0.2"} {
				requireNoLine(t, readLines(f.operationLog), op)
			}
			next := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"})
			requireSuccessResult(t, next)
			requireContains(t, next.stdout, "no fence or write was needed")
		})
	}
}

func TestEmptyHistoricalNodePhaseCannotSelectNoWritePath(t *testing.T) {
	f := newFixture(t)
	requireSuccessResult(t, f.runHelper(validConfig(), nil, map[string]string{"FAKE_TALOS_NODES_CURRENT": "true"}))
	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":      "true",
		"FAKE_EMPTY_PHASE_PRESENT_NODE": "prod-worker-1",
	})
	if result.exitCode == 0 {
		t.Fatal("an ambiguous present-but-empty phase was accepted as converged")
	}
	requireContains(t, result.stderr, "historical phase")
	if pathExists(f.talosLog) {
		t.Fatal("ambiguous phase caused Talos work")
	}
}

func TestHistoricalNodePhaseCleanupPreservesConcurrentOwner(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1"), []byte("mutating"), 0600); err != nil {
		t.Fatal(err)
	}
	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":                  "true",
		"FAKE_OWNER_BEFORE_HISTORICAL_CLEANUP_NODE": "prod-worker-1",
	})
	if result.exitCode == 0 {
		t.Fatal("cleanup accepted a concurrent ownership change")
	}
	if got := mustReadFile(filepath.Join(f.syncStateDir, "cordon-owner-prod-worker-1")); got != "successor" {
		t.Fatalf("successor ownership changed: %q", got)
	}
	if !pathExists(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1")) {
		t.Fatal("cleanup removed the successor phase")
	}
	requireNoLine(t, readLines(f.operationLog), "historical-phase-cleanup:prod-worker-1")
}

func TestHistoricalNodePhaseCleanupAcceptsLostResponseOnlyAfterReadback(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1"), []byte("mutating"), 0600); err != nil {
		t.Fatal(err)
	}
	result := f.runHelperPreservingClusterState(validConfig(), nil, map[string]string{
		"FAKE_TALOS_NODES_CURRENT":                   "true",
		"FAKE_LOST_HISTORICAL_CLEANUP_RESPONSE_NODE": "prod-worker-1",
	})
	requireSuccessResult(t, result)
	if pathExists(filepath.Join(f.syncStateDir, "cordon-phase-prod-worker-1")) {
		t.Fatal("readback accepted an uncleared phase")
	}
}
