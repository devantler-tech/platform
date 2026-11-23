package main

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func nativeSeedBackup(record benchmarkRecord) (object, error) {
	if record.Version != 3 || record.Phase != "v1" || record.Namespace != benchmarkNamespace || record.Workload != nativeSeedWorkload() ||
		!validBenchmarkOwner(record.Run, record.Source) || len(record.Resources) != 7 || validateBenchmarkResources(record) != nil {
		return nil, errSeedEvidence
	}
	return object{"apiVersion": "velero.io/v1", "kind": "Backup", "metadata": object{"name": "longhorn-v2-seed-" + record.Run, "namespace": "velero",
		"labels": object{"storage.platform.devantler.tech/benchmark-run": record.Run}}, "spec": object{
		"includedNamespaces": []any{benchmarkNamespace}, "includedResources": []any{"pods", "persistentvolumeclaims"}, "includeClusterResources": false,
		"labelSelector":   object{"matchLabels": object{"storage.platform.devantler.tech/benchmark-run": record.Run}},
		"storageLocation": "default", "snapshotVolumes": false, "snapshotMoveData": false, "defaultVolumesToFsBackup": false, "ttl": "168h0m0s", "itemOperationTimeout": "20m0s"}}, nil
}

func verifySeedBackupObject(actual, desired object, uid string) error {
	if uid == "" || text(actual, "apiVersion") != "velero.io/v1" || text(actual, "kind") != "Backup" || !seedVeleroIdentity(actual) ||
		text(actual, "metadata", "uid") != uid || text(actual, "metadata", "name") != text(desired, "metadata", "name") ||
		text(actual, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != text(desired, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") {
		return errSeedEvidence
	}
	spec, _ := value(desired, "spec").(object)
	for field, expected := range spec {
		if !reflect.DeepEqual(value(actual, "spec", field), expected) {
			return errSeedEvidence
		}
	}
	for _, forbidden := range []string{"resourcePolicy", "hooks", "orderedResources", "volumeSnapshotLocations", "orLabelSelectors"} {
		if value(actual, "spec", forbidden) != nil {
			return errSeedEvidence
		}
	}
	return nil
}

func (p *nativeSeedRun) originalBackupInputs(ctx context.Context) error {
	location, err := p.c.get(ctx, "backupstoragelocations.velero.io", "velero", "default", false)
	if err != nil {
		return err
	}
	schedules, err := p.c.list(ctx, "schedules.velero.io", "velero")
	if err != nil {
		return err
	}
	identity, digest, err := verifySeedBackupInputs(location, schedules, time.Now())
	if err != nil || identity != p.state.Location || digest != p.state.LocationSpec {
		return errSeedEvidence
	}
	return nil
}

func (p *nativeSeedRun) backup(ctx context.Context) error {
	if err := p.originalBackupInputs(ctx); err != nil {
		return err
	}
	digest, err := p.verifyPayload(ctx)
	if err != nil || digest != p.state.PayloadSHA256 {
		return errSeedEvidence
	}
	desired, err := nativeSeedBackup(p.record)
	if err != nil {
		return err
	}
	after := p.state
	after.Phase = "backup-create-requested"
	if err = p.advance(ctx, after); err != nil {
		return err
	}
	created, err := p.c.create(ctx, p.record.Source, desired)
	if err != nil {
		return errors.New("the source backup CREATE response was not confirmed; preserve its intent without adopting or retrying")
	}
	after = p.state
	after.Phase, after.Backup = "backup-owned", benchmarkIdentity(created)
	if err = p.advance(ctx, after); err != nil {
		return err
	}
	after, err = p.waitForVerifiedBackup(ctx, desired)
	if err != nil {
		return err
	}
	return p.advance(ctx, after)
}

func (p *nativeSeedRun) waitForVerifiedBackup(ctx context.Context, desired object) (canarySeedState, error) {
	var transfer, repository object
	var snapshot string
	err := waitReadyWithin(ctx, 20*time.Minute, func(ctx context.Context) error {
		parent, err := p.c.get(ctx, "backups.velero.io", "velero", p.state.Backup.Name, false)
		if err != nil {
			return err
		}
		if err = verifySeedBackupObject(parent, desired, p.state.Backup.UID); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		if err = cleanVeleroResult(parent); err != nil {
			return err
		}
		pod, err := p.originalPod(ctx)
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
		var proof canaryRun
		snapshot, err = proof.verifyFilesystemTransfer(repositories, transfers, parent, pod, "", benchmarkNamespace)
		if err != nil {
			return err
		}
		for _, candidate := range transfers {
			if controlledBy(candidate, parent) {
				transfer = candidate
			}
		}
		repository = proof.kopiaRepository
		return p.originalBackupInputs(ctx)
	})
	if err != nil {
		return canarySeedState{}, err
	}
	repositorySpec, err := seedSpecDigest(repository)
	if err != nil {
		return canarySeedState{}, err
	}
	after := p.state
	after.Phase, after.Transfer, after.Repository = "backup-verified", benchmarkIdentity(transfer), benchmarkIdentity(repository)
	after.RepositorySpec, after.Snapshot = repositorySpec, snapshot
	return after, nil
}

func (p *nativeSeedRun) retireSource(ctx context.Context) error {
	if p.state.Phase != "backup-verified" {
		return errSeedEvidence
	}
	if err := p.retainedBackup(ctx); err != nil {
		return err
	}
	digest, err := p.verifyPayload(ctx)
	if err != nil || digest != p.state.PayloadSHA256 {
		return errSeedEvidence
	}
	after := p.state
	after.Phase = "source-retirement-requested"
	if err = p.advance(ctx, after); err != nil {
		return err
	}
	if err = cleanupBenchmark(ctx, p.c, &p.record, p.created, p.claim, p.pv, p.volume); err != nil {
		return err
	}
	if err = benchmarkTags(ctx, p.c, p.record.Source, benchmarkTag("v1", p.record.Run), p.record.TagNodes, false); err != nil {
		return err
	}
	if err = p.retainedBackup(ctx); err != nil {
		return err
	}
	after = p.state
	after.Phase, after.CompletedAt = "source-cleaned", time.Now().UTC().Format(time.RFC3339Nano)
	return p.advance(ctx, after)
}
