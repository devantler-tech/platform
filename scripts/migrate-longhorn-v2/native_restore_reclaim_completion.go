package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Before creating another disposable recovery workload, bind the completed
// restore and reclaim to their actual Actions runs and live storage records.
func verifyReclaimedNativeRestore(ctx context.Context, c client, currentRun string) error {
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, c)
	executor := text(journal, "metadata", "annotations", nativeRestoreExecutorAnnotation)
	if err != nil || !nativeRestoreExecutorMatches(journal, executor) ||
		verifyNativeRestoreSuccess(ctx, c, state, executor, currentRun) != nil {
		return errNativeRestoreEvidence
	}
	restore := nativeRestoreReclaimInputs{journal: benchmarkIdentity(journal), record: record, seed: seed, state: state, executor: executor}
	cm, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreReclaimJournal, false)
	if err != nil {
		return err
	}
	progress, err := decodeNativeRestoreReclaimCompletion(cm, restore, currentRun)
	if err != nil {
		return err
	}
	producer, err := readFinishedMigrationRun(ctx, c, progress.Run, progress.Source)
	if err != nil || producer.Attempt != 1 || producer.Conclusion != "success" {
		return errNativeRestoreEvidence
	}
	graph, err := readNativeRestoreStorageGraph(ctx, c)
	if err != nil || verifyNativeRestoreStorageAbsent(state.Resources, graph) != nil {
		return errNativeRestoreEvidence
	}
	claim := state.Resources["PersistentVolumeClaim"]
	if live, err := c.get(ctx, "persistentvolumeclaim", claim.Namespace, claim.Name, true); err != nil || live != nil {
		return errNativeRestoreEvidence
	}
	current, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreReclaimJournal, false)
	if err != nil || benchmarkIdentity(current) != benchmarkIdentity(cm) ||
		text(current, "metadata", "resourceVersion") != text(cm, "metadata", "resourceVersion") {
		return errNativeRestoreEvidence
	}
	return restore.rebind(ctx, c)
}

func decodeNativeRestoreReclaimCompletion(cm object, restore nativeRestoreReclaimInputs, currentRun string) (nativeRestoreReclaimProgress, error) {
	var progress nativeRestoreReclaimProgress
	id := benchmarkIdentity(cm)
	raw := text(cm, "data", "reclaim")
	if id.Name != nativeRestoreReclaimJournal || id.Namespace != "longhorn-system" || !nativeRestoreUID(id.UID) ||
		id.UID == restore.journal.UID || text(cm, "apiVersion") != "v1" || text(cm, "kind") != "ConfigMap" ||
		text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil ||
		len(valueObject(cm, "data")) != 1 || len(raw) > 32<<10 || strictjson.Validate([]byte(raw), nil) != nil {
		return progress, errNativeRestoreEvidence
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	_, executorRun, _ := strings.Cut(restore.executor, ":")
	if decoder.Decode(&progress) != nil || progress.Phase != "completed" || !validBenchmarkOwner(progress.Run, progress.Source) ||
		progress.Run == currentRun || progress.Run == executorRun || progress.Run == restore.state.Run ||
		progress.OriginalRun != restore.state.Run || progress.RestoreJournalUID != restore.journal.UID ||
		progress.RestoreExecutor != restore.executor {
		return nativeRestoreReclaimProgress{}, errNativeRestoreEvidence
	}
	return progress, nil
}
