package main

import (
	"context"
	"os"
	"strings"
)

func nativeRestoreReclaimOwnerSucceeded(ctx context.Context, c client, state nativeRestoreState, executor string) error {
	owner, err := authorizedNativeRestoreReclaimRun(os.Getenv)
	if err != nil || !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed ||
		state.Phase != "completed" || owner != state.Run {
		return errNativeRestoreEvidence
	}
	return verifyNativeRestoreSuccess(ctx, c, state, executor, os.Getenv("GITHUB_RUN_ID"))
}

// Verify the Actions receipts without acquiring any restore or reclaim rights.
func verifyNativeRestoreSuccess(ctx context.Context, c client, state nativeRestoreState, executor, currentRun string) error {
	if !commandContextActive(ctx) || c.run == nil || state.Phase != "completed" || state.Run == currentRun {
		return errNativeRestoreEvidence
	}
	run, source := state.Run, state.Source
	if executor != "" {
		var ok bool
		source, run, ok = strings.Cut(executor, ":")
		if !ok || !validBenchmarkOwner(run, source) || run == state.Run || source == state.Source ||
			run == currentRun || verifyNativeRestoreResumeOwner(ctx, c, state) != nil {
			return errNativeRestoreEvidence
		}
	}
	actual, err := readFinishedMigrationRun(ctx, c, run, source)
	if err != nil || actual.Attempt != 1 || actual.Conclusion != "success" {
		return errNativeRestoreEvidence
	}
	return nil
}
