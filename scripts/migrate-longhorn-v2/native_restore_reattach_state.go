package main

import "strings"

const (
	nativeReattachmentDeleteRequested = "reattach-unscheduled-delete-requested"
	nativeReattachmentPodDeleted      = "reattach-unscheduled-deleted"
	nativeReattachmentCreateRequested = "reattach-replacement-create-requested"
	nativeReattachmentReplacement     = "reattach-replacement-owned"
)

// Retain the prior executor and its never-started Pod when replacing it.
type nativeReattachmentRecovery struct {
	PreviousExecutor string
	UnscheduledPod   benchmarkResource
}

func nativeReattachmentStage(phase string) int {
	switch phase {
	case nativeReattachmentDeleteRequested:
		return 1
	case nativeReattachmentPodDeleted:
		return 2
	case nativeReattachmentCreateRequested:
		return 3
	case nativeReattachmentReplacement:
		return 4
	default:
		return 0
	}
}

func validNativeReattachmentRecovery(s nativeRestoreState, seen map[string]bool) bool {
	stage := nativeReattachmentStage(s.Phase)
	if s.Reattachment == nil {
		return stage == 0
	}
	prior := s.Reattachment
	source, run, ok := strings.Cut(prior.PreviousExecutor, ":")
	pod := prior.UnscheduledPod
	if !ok || !validBenchmarkOwner(run, source) || source == s.Source || run == s.Run || s.Expired == nil ||
		pod.Name != "fio" || pod.Namespace != benchmarkNamespace || !nativeRestoreUID(pod.UID) ||
		pod.UID == s.Expired.Pod.UID {
		return false
	}
	if stage > 0 && stage < nativeReattachmentStage(nativeReattachmentReplacement) {
		return pod == s.Resources["SecondPod"]
	}
	return nativeRestorePhaseIndex(s.Phase) >= nativeRestorePhaseIndex("reattach-owned") &&
		(stage > 0 || s.Phase == "reattach-verified" || s.Phase == "completed") && !seen[pod.UID]
}
