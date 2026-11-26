package main

import (
	"context"
	"errors"
)

// prepareRecovery persists a completed backup and archived physical recovery target.
func (p *cnpgProofRun) prepareRecovery(ctx context.Context, hostname string) error {
	if p.record.Version != 1 || p.record.Phase != "backup-created" || p.record.Backup.UID == "" || p.record.Plan != nil || p.record.RestoreVerified || p.record.Cleaned {
		return errors.New("recovery planning requires the original completed backup attempt")
	}
	if _, err := waitCNPGBackup(ctx, p.c, p.record.Target, p.record.Run, p.record.Backup); err != nil {
		return err
	}
	owner, err := p.currentExecution(ctx)
	if err != nil {
		return err
	}
	request := cnpgRecoveryRequest{Target: p.record.Target, Source: owner.Source, Run: p.record.Run, Hostname: hostname, Archive: p.record.Archive, Backup: p.record.Backup}
	plan, err := planCNPGRecovery(ctx, p.c, request)
	if err != nil {
		return err
	}
	if err = waitCNPGRecoveryArchive(ctx, p.c, request, plan); err != nil {
		return err
	}
	p.record.Plan = &plan
	p.record.Phase = "restore-planned"
	return p.save(ctx)
}
