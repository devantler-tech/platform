package main

import (
	"context"
	"errors"
)

// readCNPGProof binds strictly decoded evidence to the original native journal.
// Phase completeness and executor takeover require their own subsequent checks.
func readCNPGProof(ctx context.Context, c client, target string) (object, cnpgProofRecord, error) {
	refused := errors.New("database proof journal is outside the reviewed procedure")
	selected := false
	for _, source := range cnpgArchiveSources() {
		selected = selected || source.Name == target
	}
	if !selected {
		return nil, cnpgProofRecord{}, refused
	}
	name := "longhorn-v2-db-proof-" + target
	cm, err := c.get(ctx, "configmap", "longhorn-system", name, false)
	if err != nil {
		return nil, cnpgProofRecord{}, err
	}
	record, err := decodeCNPGProofRecord([]byte(text(cm, "data", "record")))
	if err != nil || record.Version != 1 || record.Target != target || !validBenchmarkOwner(record.Run, record.Source) || text(cm, "apiVersion") != "v1" || text(cm, "kind") != "ConfigMap" || text(cm, "metadata", "name") != name || text(cm, "metadata", "namespace") != "longhorn-system" || text(cm, "metadata", "uid") == "" || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil || text(cm, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || text(cm, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != record.Run {
		return nil, cnpgProofRecord{}, refused
	}
	if owner := record.Executor; owner != nil && (!validBenchmarkOwner(owner.Run, owner.Source) || owner.Run == record.Run || owner.JournalUID != text(cm, "metadata", "uid")) {
		return nil, cnpgProofRecord{}, refused
	}
	return cm, record, nil
}
