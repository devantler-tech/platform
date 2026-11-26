package main

import (
	"context"
	"errors"
	"reflect"
	"time"
)

// waitCNPGRecoveryArchive requires the exact target's native completion marker.
func waitCNPGRecoveryArchive(ctx context.Context, c client, r cnpgRecoveryRequest, plan cnpgRecoveryPlan) error {
	refused := errors.New("database archive completion lacks a stable recorded source")
	if c.context != "admin@prod" || plan.Source.Cluster != r.Target || plan.Source.BackupUID != r.Backup.UID || r.Backup.UID == "" || validPostgresState(plan.Barrier) != nil || plan.Barrier.InRecovery {
		return refused
	}
	before, err := observeCNPGMigration(ctx, c, r.Target, r.Source)
	if err != nil {
		return err
	}
	primaryName := text(before.Cluster, "status", "currentPrimary")
	initial := before.States[primaryName]
	wanted, _ := walPosition(plan.Barrier.FlushLSN)
	minimum, _ := walPosition(initial.FlushLSN)
	if text(before.Cluster, "metadata", "uid") != plan.Source.ClusterUID || initial.SystemID != plan.Barrier.SystemID || initial.Timeline != plan.Barrier.Timeline || initial.InRecovery || minimum < wanted {
		return refused
	}
	var primary object
	for _, pod := range before.Pods {
		if text(pod, "metadata", "name") == primaryName {
			primary = pod
		}
	}
	if primary == nil {
		return refused
	}
	store, err := c.get(ctx, "objectstores.barmancloud.cnpg.io", plan.Source.Namespace, plan.Source.Store, false)
	if err != nil {
		return err
	}
	if _, err = bindCNPGArchiveSource(before.Cluster, store, r.Archive); err != nil {
		return err
	}
	err = waitReadyWithin(ctx, 8*time.Minute, func(sample context.Context) error {
		if e := sourceCurrent(sample, c.run, r.Source); e != nil {
			return errors.Join(errReadinessTerminal, e)
		}
		raw, e := c.readPostgresSQL(sample, before.Cluster, primary, plan.Barrier.FlushLSN)
		if e != nil {
			return errors.Join(errReadinessTerminal, e)
		}
		done, e := verifyCNPGArchiveMarker(raw, plan.Barrier)
		if e != nil {
			return errors.Join(errReadinessTerminal, e)
		}
		current, e := parsePostgresState(raw)
		if e != nil {
			return errors.Join(errReadinessTerminal, e)
		}
		position, _ := walPosition(current.FlushLSN)
		if position < minimum {
			return errors.Join(errReadinessTerminal, refused)
		}
		minimum = position
		if !done {
			return errors.New("the exact recovery WAL segment is not positively archived")
		}
		return nil
	})
	if err != nil {
		return err
	}
	after, err := observeCNPGMigration(ctx, c, r.Target, r.Source)
	if err != nil {
		return err
	}
	if err = validateCNPGObservation(before, after); err != nil {
		return err
	}
	finalPosition, _ := walPosition(after.States[primaryName].FlushLSN)
	if finalPosition < minimum {
		return refused
	}
	store, err = c.get(ctx, "objectstores.barmancloud.cnpg.io", plan.Source.Namespace, plan.Source.Store, false)
	if err != nil {
		return err
	}
	backup, err := c.get(ctx, "backups.postgresql.cnpg.io", r.Backup.Namespace, r.Backup.Name, false)
	if err != nil {
		return err
	}
	source, err := bindCNPGRecoverySource(after.Cluster, store, backup, r.Backup.UID, r.Archive)
	if err != nil || benchmarkIdentity(backup) != r.Backup || text(backup, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != r.Run || text(backup, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || !reflect.DeepEqual(source, plan.Source) || verifyCNPGBackupBarrier(backup, plan.Barrier) != nil {
		return refused
	}
	return sourceCurrent(ctx, c.run, r.Source)
}
