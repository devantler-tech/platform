package main

import (
	"context"
	"errors"
	"time"
)

// waitCNPGBackup accepts only positive completion of the recorded native object.
func waitCNPGBackup(ctx context.Context, c client, target, run string, expected benchmarkResource) (object, error) {
	var source cnpgSource
	for _, candidate := range cnpgArchiveSources() {
		if candidate.Name == target {
			source = candidate
		}
	}
	if source.Name == "" || !recoveryRun.MatchString(run) || expected.Name != "longhorn-v2-proof-"+target+"-"+run || expected.Namespace != source.Namespace || expected.UID == "" {
		return nil, errors.New("backup completion requires the recorded fixed source identity")
	}
	var completed object
	err := waitReadyWithin(ctx, 20*time.Minute, func(sample context.Context) error {
		backup, err := c.get(sample, "backups.postgresql.cnpg.io", expected.Namespace, expected.Name, false)
		if err != nil {
			return err
		}
		if benchmarkIdentity(backup) != expected || value(backup, "metadata", "deletionTimestamp") != nil || text(backup, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != run || text(backup, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || text(backup, "spec", "cluster", "name") != target || text(backup, "spec", "method") != "plugin" || text(backup, "spec", "pluginConfiguration", "name") != "barman-cloud.cloudnative-pg.io" {
			return errors.Join(errReadinessTerminal, errors.New("recorded backup identity or source changed"))
		}
		if text(backup, "status", "phase") == "failed" {
			return errors.Join(errReadinessTerminal, errors.New("recorded native backup failed"))
		}
		if text(backup, "status", "phase") != "completed" {
			return errors.New("recorded native backup is not completed")
		}
		completed = backup
		return nil
	})
	if err != nil {
		return nil, err
	}
	return completed, nil
}
