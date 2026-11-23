package main

import "context"

func verifyRetainedSeedBackup(record benchmarkRecord, state canarySeedState, backup, pod object, transfers, repositories []object) error {
	if state.validate(record) != nil || seedPhaseIndex(state.Phase) < seedPhaseIndex("backup-verified") || benchmarkIdentity(pod) != record.Resources["Pod"] {
		return errSeedEvidence
	}
	desired, err := nativeSeedBackup(record)
	if err != nil || verifySeedBackupObject(backup, desired, state.Backup.UID) != nil || cleanVeleroResult(backup) != nil {
		return errSeedEvidence
	}
	var originalRepository object
	for _, candidate := range repositories {
		if benchmarkIdentity(candidate) == state.Repository {
			digest, err := seedSpecDigest(candidate)
			if err != nil || digest != state.RepositorySpec || originalRepository != nil {
				return errSeedEvidence
			}
			originalRepository = candidate
		}
	}
	if originalRepository == nil {
		return errSeedEvidence
	}
	proof := canaryRun{kopiaRepository: originalRepository}
	snapshot, err := proof.verifyFilesystemTransfer(repositories, transfers, backup, pod, "", benchmarkNamespace)
	if err != nil || snapshot != state.Snapshot {
		return errSeedEvidence
	}
	for _, transfer := range transfers {
		if controlledBy(transfer, backup) {
			if benchmarkIdentity(transfer) != state.Transfer {
				return errSeedEvidence
			}
			return nil
		}
	}
	return errSeedEvidence
}

// Re-read the original remote-data graph before source retirement and again
// after local cleanup. A completed Backup alone is never transfer evidence.
func (p *nativeSeedRun) retainedBackup(ctx context.Context) error {
	if err := p.originalBackupInputs(ctx); err != nil {
		return err
	}
	backup, err := p.c.get(ctx, "backups.velero.io", "velero", p.state.Backup.Name, false)
	if err != nil {
		return err
	}
	transfers, err := p.c.list(ctx, "podvolumebackups.velero.io", "velero")
	if err != nil {
		return err
	}
	repositories, err := p.c.list(ctx, "backuprepositories.velero.io", "velero")
	if err != nil {
		return err
	}
	return verifyRetainedSeedBackup(p.record, p.state, backup, p.created["Pod"], transfers, repositories)
}
