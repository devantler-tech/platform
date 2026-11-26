package main

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func testPendingReattachState(t *testing.T) (benchmarkRecord, canarySeedState, nativeRestoreState) {
	t.Helper()
	record, seed, state := nativeRestoreStateFixture(t, "reattach-owned")
	state.Expired = &nativeRestoreExpiredReceipt{
		Pod:      benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "original-expired-pod"},
		Transfer: state.Resources["PodVolumeRestore"], Replacement: state.Resources["FirstPod"],
	}
	return record, seed, state
}

func TestReattachmentRecoveryJournalPreservesMarkerAndOriginalConsumers(t *testing.T) {
	record, seed, old := testPendingReattachState(t)
	deleting := old
	deleting.Phase = nativeReattachmentDeleteRequested
	deleting.Reattachment = &nativeReattachmentRecovery{
		PreviousExecutor: strings.Repeat("a", 40) + ":9876", UnscheduledPod: old.Resources["SecondPod"],
	}
	deleted := deleting
	deleted.Phase = nativeReattachmentPodDeleted
	creating := deleted
	creating.Phase = nativeReattachmentCreateRequested
	replaced := creating
	replaced.Phase = nativeReattachmentReplacement
	replaced.Resources = maps.Clone(creating.Resources)
	replaced.Resources["SecondPod"] = benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "replacement-reattachment-pod"}
	verified := replaced
	verified.Phase, verified.SecondNode = "reattach-verified", old.Targets[2].Name
	for _, pair := range [][2]nativeRestoreState{{old, deleting}, {deleting, deleted}, {deleted, creating}, {creating, replaced}, {replaced, verified}} {
		if err := validateNativeRestoreTransition(pair[0], pair[1], record, seed); err != nil {
			t.Fatalf("%s to %s: %v", pair[0].Phase, pair[1].Phase, err)
		}
		data, err := json.Marshal(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeNativeRestoreState(string(data), record, seed); err != nil {
			t.Fatal("stored journal:", err)
		}
	}
	changedMarker := verified
	changedMarker.Token = strings.Repeat("f", 64)
	changedPrior := deleted
	drift := *changedPrior.Reattachment
	drift.PreviousExecutor = strings.Repeat("a", 40) + ":9999"
	changedPrior.Reattachment = &drift
	for _, pair := range [][2]nativeRestoreState{{old, replaced}, {deleting, changedPrior}, {deleting, verified}, {creating, verified}, {replaced, changedMarker}} {
		if validateNativeRestoreTransition(pair[0], pair[1], record, seed) == nil {
			t.Fatalf("accepted a changed or skipped recovery from %s to %s", pair[0].Phase, pair[1].Phase)
		}
	}
}

func TestReattachmentRecoveryRefusesAnythingThatMayHaveRun(t *testing.T) {
	record, seed, state := testPendingReattachState(t)
	targets := map[string]string{}
	for _, target := range state.Targets {
		targets[target.Name] = target.UID
	}
	asset, err := nativeRestoreReattachPod(record, seed, nativeReattachRequest{
		Run: state.Run, Token: state.Token, FirstNode: state.FirstNode, Target: state.Targets[1].Name, Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := valueObject(asset, "metadata")
	metadata["uid"], metadata["resourceVersion"] = state.Resources["SecondPod"].UID, "453"
	asset["status"] = object{"phase": "Pending", "conditions": []any{object{
		"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/9 nodes are available: 1 Insufficient memory.",
	}}}
	data, err := json.Marshal(asset)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "node assigned", "wrong failure", "starting", "foreign claim", "changed memory", "another uid"} {
		t.Run(name, func(t *testing.T) {
			var pod object
			if err := json.Unmarshal(data, &pod); err != nil {
				t.Fatal(err)
			}
			spec, status := valueObject(pod, "spec"), valueObject(pod, "status")
			switch name {
			case "node assigned":
				spec["nodeName"] = state.Targets[1].Name
			case "wrong failure":
				status["conditions"].([]any)[0].(object)["message"] = "VolumeConflict"
			case "starting":
				status["startTime"] = "2026-11-26T11:43:00Z"
			case "foreign claim":
				valueObject(spec["volumes"].([]any)[0].(object), "persistentVolumeClaim")["claimName"] = "different"
			case "changed memory":
				valueObject(spec["containers"].([]any)[0].(object), "resources", "requests")["memory"] = "32Mi"
			case "another uid":
				valueObject(pod, "metadata")["uid"] = "replacement"
			}
			if err := verifyUnscheduledReattachment(pod, state, seed); (err == nil) != (name == "") {
				t.Fatal("unexpected unassigned-consumer verdict", err)
			}
		})
	}
}
func TestReattachmentSelectsAnotherQualifiedWorkerWithMemory(t *testing.T) {
	record, seed, state := testPendingReattachState(t)
	var live inventory
	for _, target := range state.Targets {
		node := resource(target.Name)
		valueObject(node, "metadata")["uid"] = target.UID
		node["status"] = object{
			"capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi", "memory": "12Gi"},
			"conditions": []any{
				object{"type": "Ready", "status": "True"}, object{"type": "MemoryPressure", "status": "False"},
				object{"type": "DiskPressure", "status": "False"}, object{"type": "PIDPressure", "status": "False"},
			},
		}
		live.nodes = append(live.nodes, node)
	}
	live.pods = []object{{"spec": object{"nodeName": state.Targets[1].Name, "containers": []any{
		object{"resources": object{"requests": object{"memory": "11900Mi"}}},
	}}, "status": object{"phase": "Running"}}}
	pod, err := selectReattachmentConsumer(record, seed, state, live, "")
	if err != nil || text(pod, "spec", "nodeSelector", "kubernetes.io/hostname") != state.Targets[2].Name {
		t.Fatal("selection ignored scheduler memory accounting", err)
	}
	if _, err := selectReattachmentConsumer(record, seed, state, live, state.Targets[2].Name); err == nil {
		t.Fatal("selection admitted the first worker, the excluded peer or the saturated peer")
	}
	valueObject(live.nodes[2], "metadata")["uid"] = "replaced-node"
	if _, err := selectReattachmentConsumer(record, seed, state, live, ""); err == nil {
		t.Fatal("selection admitted a peer with changed identity")
	}
}

func TestReattachmentRecoveryMainEntryPoint(t *testing.T) {
	const helper = "PLATFORM_TEST_REATTACH_RECOVERY_MAIN"
	if os.Getenv(helper) == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	_, _, state := testPendingReattachState(t)
	env := nativeRestoreDispatch(state)
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = nativeReattachmentOperation, nativeReattachmentConfirmation+state.Run+":9876"
	env["GITHUB_SHA"], env["GITHUB_RUN_ID"] = strings.Repeat("b", 40), "9877"
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReattachmentRecoveryMainEntryPoint$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), helper+"=1")
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "Reviewed capacity configuration is unreadable." {
		t.Fatalf("confirmed recovery failed the executable boundary: %v: %s", err, output)
	}
	for _, bad := range []string{state.Run, state.Run + ":", state.Run + ":9877", state.Run + ":" + state.Run, "other:9876", state.Run + ":9876:1"} {
		env["LONGHORN_V2_CONFIRM"] = nativeReattachmentConfirmation + bad
		if _, _, err := authorizeNativeReattachment(func(key string) string { return env[key] }); err == nil {
			t.Fatal("accepted malformed ownership", bad)
		}
	}
}
