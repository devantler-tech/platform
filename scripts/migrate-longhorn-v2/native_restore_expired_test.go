package main

import (
	"encoding/json"
	"maps"
	"testing"
)

func TestNativeRestoreExpiredTransferPreservesOriginalSource(t *testing.T) {
	for _, fault := range []string{"", "wrong deadline", "wrong reason", "still running", "wrong claim", "wrong snapshot", "restarted"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, state, restore, pod, transfer, repository := nativeRestoreTransferFixture(t)
			valueObject(pod, "spec")["activeDeadlineSeconds"] = float64(2700)
			valueObject(pod, "spec")["restartPolicy"] = "Never"
			valueObject(pod, "status")["phase"] = "Failed"
			valueObject(pod, "status")["reason"] = "DeadlineExceeded"
			valueObject(pod, "status")["startTime"] = "2026-11-15T10:00:00Z"
			valueObject(pod, "status")["conditions"] = []any{object{"type": "Ready", "status": "False"}}
			process := object{"name": "fio", "ready": false, "restartCount": float64(0), "state": object{
				"terminated": object{"exitCode": float64(137), "startedAt": "2026-11-15T10:00:30Z", "finishedAt": "2026-11-15T10:45:15Z"},
			}}
			valueObject(pod, "status")["containerStatuses"] = []any{process}
			switch fault {
			case "wrong deadline":
				valueObject(pod, "spec")["activeDeadlineSeconds"] = float64(300)
			case "wrong reason":
				valueObject(pod, "status")["reason"] = "Evicted"
			case "still running":
				process["state"] = object{"running": object{}}
			case "wrong claim":
				valueObject(value(pod, "spec", "volumes").([]any)[0].(object), "persistentVolumeClaim")["claimName"] = "other"
			case "wrong snapshot":
				valueObject(transfer, "spec")["snapshotID"] = "other"
			case "restarted":
				process["restartCount"] = float64(1)
			}
			receipt, err := verifyExpiredNativeRestoreTransfer(record, seed, state, restore, pod, []object{transfer}, []object{repository})
			if (err == nil) != (fault == "") || err == nil && (receipt.Pod != benchmarkIdentity(pod) || receipt.Transfer != benchmarkIdentity(transfer)) {
				t.Fatalf("unexpected expired transfer verdict: %+v %v", receipt, err)
			}
		})
	}
}

func TestNativeRestoreExpiredConsumerTransitionPreservesVeleroAndBothAttachments(t *testing.T) {
	record, seed, original := nativeRestoreStateFixture(t, "restore-owned")
	expired := nativeRestoreExpiredReceipt{
		Pod:      benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "original-velero-pod"},
		Transfer: benchmarkResource{Name: "pod-volume-restore", Namespace: "velero", UID: "original-transfer"},
	}
	deleting := original
	deleting.Phase, deleting.Expired = nativeExpiredDeleteRequested, &expired
	deleted := deleting
	deleted.Phase = nativeExpiredPodDeleted
	creating := deleted
	creating.Phase = nativeExpiredCreateRequested
	own := creating
	replacement := expired
	replacement.Replacement = benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "replacement-pod"}
	own.Phase, own.Expired = nativeExpiredReplacement, &replacement
	verified := own
	verified.Phase, verified.FirstNode = "restore-verified", original.Targets[0].Name
	verified.Resources = maps.Clone(original.Resources)
	verified.Resources["FirstPod"], verified.Resources["PodVolumeRestore"] = replacement.Replacement, replacement.Transfer
	for _, pair := range [][2]nativeRestoreState{{original, deleting}, {deleting, deleted}, {deleted, creating}, {creating, own}, {own, verified}} {
		if err := validateNativeRestoreTransition(pair[0], pair[1], record, seed); err != nil {
			t.Fatalf("valid expired recovery transition refused from %s to %s: %v", pair[0].Phase, pair[1].Phase, err)
		}
		data, err := json.Marshal(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeNativeRestoreState(string(data), record, seed); err != nil {
			t.Fatal("the validated state failed to survive journal readback", err)
		}
	}
	corrupt := deleted
	other := expired
	other.Transfer.UID = "borrowed-transfer"
	corrupt.Expired = &other
	wrongFirst := verified
	wrongFirst.Resources = maps.Clone(verified.Resources)
	wrongFirst.Resources["FirstPod"] = expired.Pod
	for _, pair := range [][2]nativeRestoreState{
		{original, deleted}, {deleting, corrupt}, {deleted, own}, {creating, verified}, {own, wrongFirst}, {own, own}, {verified, original},
	} {
		if err := validateNativeRestoreTransition(pair[0], pair[1], record, seed); err == nil {
			t.Fatalf("unsafe expired recovery transition admitted from %s to %s", pair[0].Phase, pair[1].Phase)
		}
	}
}

func TestNativeRestoreExpiredReplacementRunsWithoutTheVeleroHelper(t *testing.T) {
	record, seed, state, pod, _ := nativeRestoreConsumerFixture(t, false)
	state.Expired = &nativeRestoreExpiredReceipt{
		Pod:         benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "original-expired-pod"},
		Transfer:    state.Resources["PodVolumeRestore"],
		Replacement: state.Resources["FirstPod"],
	}
	delete(valueObject(pod, "spec"), "initContainers")
	delete(valueObject(pod, "status"), "initContainerStatuses")
	delete(valueObject(pod, "metadata", "labels"), "velero.io/restore-name")
	if err := verifyNativeRestoreConsumer(record, seed, state, pod, ""); err != nil {
		t.Fatal("the first replacement should read the original bytes without launching Velero", err)
	}
	valueObject(pod, "metadata", "labels")["velero.io/restore-name"] = state.Resources["Restore"].Name
	if err := verifyNativeRestoreConsumer(record, seed, state, pod, ""); err == nil {
		t.Fatal("the replacement was allowed to impersonate the original Velero Pod")
	}
}
