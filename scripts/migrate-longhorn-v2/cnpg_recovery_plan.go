package main

import (
	"context"
	"errors"
	"reflect"
)

type cnpgRecoveryRequest struct {
	Target, Source, Run, Hostname string
	Archive                       cnpgArchiveBinding
	Backup                        benchmarkResource
}

type cnpgRecoveryPlan struct {
	Source    cnpgRecoverySource
	Barrier   postgresState
	Resources []object
}

// planCNPGRecovery reads a completed recorded backup before capturing its recovery target.
func planCNPGRecovery(ctx context.Context, c client, r cnpgRecoveryRequest) (cnpgRecoveryPlan, error) {
	refused := errors.New("database recovery plan lacks its recorded backup or current native source")
	var selected cnpgSource
	for _, source := range cnpgArchiveSources() {
		if source.Name == r.Target {
			selected = source
		}
	}
	if selected.Name == "" || !recoveryRun.MatchString(r.Run) || r.Backup.Name != "longhorn-v2-proof-"+r.Target+"-"+r.Run || r.Backup.Namespace != selected.Namespace || r.Backup.UID == "" {
		return cnpgRecoveryPlan{}, refused
	}
	backup, err := c.get(ctx, "backups.postgresql.cnpg.io", selected.Namespace, r.Backup.Name, false)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	if benchmarkIdentity(backup) != r.Backup || value(backup, "metadata", "deletionTimestamp") != nil || text(backup, "status", "phase") != "completed" || text(backup, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != r.Run || text(backup, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return cnpgRecoveryPlan{}, refused
	}
	// This physical sample follows positive completion of the recorded Backup.
	// A caller cannot select a pre-backup recovery target from an earlier read.
	observation, err := observeCNPGMigration(ctx, c, r.Target, r.Source)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	storeName := r.Target
	if r.Target == "wedding-db" {
		storeName = "wedding-db-dedicated"
	}
	store, err := c.get(ctx, "objectstores.barmancloud.cnpg.io", selected.Namespace, storeName, false)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	latest, err := c.get(ctx, "backups.postgresql.cnpg.io", selected.Namespace, r.Backup.Name, false)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	if benchmarkIdentity(latest) != r.Backup || !reflect.DeepEqual(value(latest, "spec"), value(backup, "spec")) || !reflect.DeepEqual(value(latest, "status"), value(backup, "status")) || !reflect.DeepEqual(value(latest, "metadata", "labels"), value(backup, "metadata", "labels")) || value(latest, "metadata", "deletionTimestamp") != nil {
		return cnpgRecoveryPlan{}, refused
	}
	source, err := bindCNPGRecoverySource(observation.Cluster, store, latest, r.Backup.UID, r.Archive)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	primary := text(observation.Cluster, "status", "currentPrimary")
	barrier := observation.States[primary]
	if err = verifyCNPGBackupBarrier(latest, barrier); err != nil {
		return cnpgRecoveryPlan{}, err
	}
	var process postgresProcess
	for _, pod := range observation.Pods {
		if text(pod, "metadata", "name") == primary {
			process, err = postgresExecutable(pod)
		}
	}
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	resources, err := cnpgRecoveryResources(r.Run, source, process.Image, barrier, r.Hostname)
	if err != nil {
		return cnpgRecoveryPlan{}, err
	}
	return cnpgRecoveryPlan{Source: source, Barrier: barrier, Resources: resources}, nil
}
