package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

func bindNativeRestoreReclaimContinuation(inputs nativeRestoreReclaimInputs, prior nativeRestoreReclaimProgress, failed, current, source string) (nativeRestoreReclaimProgress, error) {
	refused := errors.New("the failed reclaim did not stop at the two original bound fences")
	pv := "lh-native-restore-reclaim-pv-" + inputs.state.Run
	pvc := "lh-native-restore-reclaim-pvc-" + inputs.state.Run
	if prior.PreviousAttempt != "" || prior.Run != failed || failed == current || failed == inputs.state.Run ||
		current == inputs.state.Run || !validBenchmarkOwner(failed, prior.Source) || !validBenchmarkOwner(current, source) ||
		prior.Source == source || prior.OriginalRun != inputs.state.Run || prior.RestoreJournalUID != inputs.journal.UID ||
		prior.RestoreExecutor != inputs.executor || prior.Phase != "bound/"+pvc || len(prior.Fences) != 2 {
		return nativeRestoreReclaimProgress{}, refused
	}
	seen := nativeRestoreReceiptUIDs(inputs.record, inputs.seed, inputs.state)
	for _, name := range []string{pv, pvc} {
		fence := prior.Fences[name]
		for _, uid := range []string{fence.PolicyUID, fence.BindingUID} {
			if uid == inputs.journal.UID || !nativeRestoreUID(uid) || seen[uid] {
				return nativeRestoreReclaimProgress{}, refused
			}
			seen[uid] = true
		}
	}
	prior.PreviousAttempt = prior.Source + ":" + prior.Run
	prior.Source, prior.Run = source, current
	return prior, nil
}

func observeNativeRestoreReclaimContinuation(ctx context.Context, c client, inputs nativeRestoreReclaimInputs) (nativeRestoreReclaim, object, error) {
	refused := errors.New("the original reclaim journal or failed Actions attempt is unverified")
	original, failed, valid := nativeRestoreReclaimResumeRuns(os.Getenv)
	if !valid || original != inputs.state.Run || os.Getenv("LONGHORN_V2_OPERATION") != nativeRestoreReclaimResume {
		return nativeRestoreReclaim{}, nil, refused
	}
	journal, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreReclaimJournal, false)
	if err != nil || text(journal, "apiVersion") != "v1" || text(journal, "kind") != "ConfigMap" ||
		text(journal, "metadata", "namespace") != "longhorn-system" || text(journal, "metadata", "name") != nativeRestoreReclaimJournal ||
		!nativeRestoreUID(text(journal, "metadata", "uid")) || value(journal, "metadata", "deletionTimestamp") != nil || len(valueObject(journal, "data")) != 1 ||
		benchmarkIdentity(journal) == inputs.journal || nativeRestoreReceiptUIDs(inputs.record, inputs.seed, inputs.state)[text(journal, "metadata", "uid")] {
		return nativeRestoreReclaim{}, nil, refused
	}
	raw := text(journal, "data", "reclaim")
	var prior nativeRestoreReclaimProgress
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &prior) != nil {
		return nativeRestoreReclaim{}, nil, refused
	}
	canonical, err := json.Marshal(prior)
	if err != nil || string(canonical) != raw {
		return nativeRestoreReclaim{}, nil, refused
	}
	next, err := bindNativeRestoreReclaimContinuation(inputs, prior, failed, os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_SHA"))
	if err != nil {
		return nativeRestoreReclaim{}, nil, err
	}
	old, err := readFinishedMigrationRun(ctx, c, failed, prior.Source)
	if err != nil || old.Attempt != 1 || old.Conclusion != "failure" {
		return nativeRestoreReclaim{}, nil, refused
	}
	pod, err := inputs.readConsumer(ctx, c)
	if err != nil || pod != nil {
		return nativeRestoreReclaim{}, nil, errors.New("the original test consumer has not been retired")
	}
	return nativeRestoreReclaim{c: c, inputs: inputs, journal: benchmarkIdentity(journal), progress: next}, journal, nil
}

func (r nativeRestoreReclaim) adoptBoundFences(ctx context.Context, prior object) error {
	if r.inputs.rebind(ctx, r.c) != nil || sourceCurrent(ctx, r.c.run, r.progress.Source) != nil {
		return errNativeRestoreEvidence
	}
	raw, err := json.Marshal(r.progress)
	if err != nil {
		return err
	}
	if err := r.c.patch(ctx, "configmap", "longhorn-system", prior,
		object{"op": "test", "path": "/data/reclaim", "value": text(prior, "data", "reclaim")},
		object{"op": "replace", "path": "/data/reclaim", "value": string(raw)}); err != nil {
		return errors.New("the existing reclaim journal handoff was not confirmed")
	}
	_, err = r.guard(ctx)
	return err
}
