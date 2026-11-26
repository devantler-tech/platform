package main

import (
	"strings"
	"testing"
)

func TestNativeRestoreReclaimContinuationRequiresTheFailedRunAndBoundFences(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "completed")
	const failedRun = "6789"
	const currentRun = "9123"
	previousSource, currentSource := strings.Repeat("a", 40), strings.Repeat("b", 40)
	pv := "lh-native-restore-reclaim-pv-" + state.Run
	pvc := "lh-native-restore-reclaim-pvc-" + state.Run
	ids := []string{
		"ac005841-1111-4111-8111-111111111111", "ac005841-2222-4222-8222-222222222222",
		"ac005841-3333-4333-8333-333333333333", "ac005841-4444-4444-8444-444444444444",
	}

	for _, fault := range []string{"", "wrong run", "later phase", "unbound fence", "reused UID", "changed restore"} {
		t.Run(fault, func(t *testing.T) {
			inputs := nativeRestoreReclaimInputs{
				state: state, journal: benchmarkResource{UID: "ac005841-5555-4555-8555-555555555555"},
				executor: strings.Repeat("c", 40) + ":7456",
			}
			prior := nativeRestoreReclaimProgress{
				Source: previousSource, Run: failedRun, OriginalRun: state.Run, RestoreJournalUID: inputs.journal.UID,
				RestoreExecutor: inputs.executor, Phase: "bound/" + pvc,
				Fences: map[string]benchmarkFenceReceipt{pv: {PolicyUID: ids[0], BindingUID: ids[1]}, pvc: {PolicyUID: ids[2], BindingUID: ids[3]}},
			}
			switch fault {
			case "wrong run":
				prior.Run = "6790"
			case "later phase":
				prior.Phase = "reclaim-policy-requested"
			case "unbound fence":
				delete(prior.Fences, pvc)
			case "reused UID":
				prior.Fences[pvc] = prior.Fences[pv]
			case "changed restore":
				prior.RestoreExecutor = ""
			}
			next, err := bindNativeRestoreReclaimContinuation(inputs, prior, failedRun, currentRun, currentSource)
			if (err == nil) != (fault == "") {
				t.Fatalf("unexpected checkpoint result: %v", err)
			}
			if err == nil && (next.Run != currentRun || next.Source != currentSource || next.Phase != prior.Phase ||
				next.PreviousAttempt != previousSource+":"+failedRun || next.Fences[pvc] != prior.Fences[pvc]) {
				t.Fatal("the continuation lost the original attempt or fences")
			}
		})
	}
}
