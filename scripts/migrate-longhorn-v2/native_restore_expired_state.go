package main

import "reflect"

const (
	nativeExpiredDeleteRequested = "expired-pod-delete-requested"
	nativeExpiredPodDeleted      = "expired-pod-deleted"
	nativeExpiredCreateRequested = "expired-replacement-create-requested"
	nativeExpiredReplacement     = "expired-replacement-owned"
)

type nativeRestoreExpiredReceipt struct {
	Pod, Transfer, Replacement benchmarkResource
}

func nativeRestoreExpiredStage(phase string) int {
	switch phase {
	case nativeExpiredDeleteRequested:
		return 1
	case nativeExpiredPodDeleted:
		return 2
	case nativeExpiredCreateRequested:
		return 3
	case nativeExpiredReplacement:
		return 4
	default:
		return 0
	}
}

func validNativeRestoreExpiredReceipt(state nativeRestoreState, seen map[string]bool) bool {
	stage := nativeRestoreExpiredStage(state.Phase)
	evidence := state.Expired
	if evidence == nil {
		return stage == 0
	}
	pod, transfer, replacement := evidence.Pod, evidence.Transfer, evidence.Replacement
	if pod.Name != "fio" || pod.Namespace != benchmarkNamespace || !nativeRestoreUID(pod.UID) || seen[pod.UID] ||
		transfer.Namespace != "velero" || !kubeName.MatchString(transfer.Name) || len(transfer.Name) > 253 ||
		!nativeRestoreUID(transfer.UID) || transfer.UID == pod.UID {
		return false
	}
	if stage > 0 {
		if seen[transfer.UID] {
			return false
		}
		if stage < nativeRestoreExpiredStage(nativeExpiredReplacement) {
			return replacement == (benchmarkResource{})
		}
		return replacement.Name == "fio" && replacement.Namespace == benchmarkNamespace && nativeRestoreUID(replacement.UID) &&
			!seen[replacement.UID] && replacement.UID != pod.UID && replacement.UID != transfer.UID
	}
	return nativeRestorePhaseIndex(state.Phase) >= nativeRestorePhaseIndex("restore-verified") &&
		state.Resources["FirstPod"] == replacement && state.Resources["PodVolumeRestore"] == transfer
}

func validNativeRestorePhaseTransition(before, after nativeRestoreState) bool {
	if nativeReattachmentStage(after.Phase) > 0 {
		if !reflect.DeepEqual(before.Expired, after.Expired) {
			return false
		}
		if after.Phase == nativeReattachmentDeleteRequested {
			return before.Phase == "reattach-owned" && before.Reattachment == nil && after.Reattachment != nil
		}
		return nativeReattachmentStage(after.Phase) == nativeReattachmentStage(before.Phase)+1 &&
			reflect.DeepEqual(before.Reattachment, after.Reattachment)
	}
	if !reflect.DeepEqual(before.Reattachment, after.Reattachment) {
		return false
	}
	if stage := nativeReattachmentStage(before.Phase); stage > 0 && stage != nativeReattachmentStage(nativeReattachmentReplacement) {
		return false
	}
	oldStage, nextStage := nativeRestoreExpiredStage(before.Phase), nativeRestoreExpiredStage(after.Phase)
	if nextStage == 1 {
		return before.Phase == "restore-owned" && before.Expired == nil
	}
	if nextStage > 1 {
		return nextStage == oldStage+1 && before.Expired != nil && after.Expired != nil &&
			before.Expired.Pod == after.Expired.Pod && before.Expired.Transfer == after.Expired.Transfer
	}
	if oldStage != 0 && oldStage != nativeRestoreExpiredStage(nativeExpiredReplacement) {
		return false
	}
	return nativeRestorePhaseIndex(after.Phase) == nativeRestorePhaseIndex(before.Phase)+1 && reflect.DeepEqual(before.Expired, after.Expired)
}
