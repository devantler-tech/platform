package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	keeperOriginalResuming  = "original-member-resuming"
	keeperOriginalPreserved = "original-claim-preserved"
)

type corootKeeperRetainedStage struct {
	PV, Volume benchmarkResource
}

func (j *corootKeeperJournal) observeRetainedKeeperStaging(ctx context.Context, r corootKeeperCutoverRecord) (corootKeeperRetainedStage, error) {
	var retained corootKeeperRetainedStage
	if r.StagedClaim.UID == "" {
		return retained, nil
	}
	if err := j.requireNoClaimConsumers(ctx, r.StagedClaim.Name); err != nil {
		return retained, err
	}
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.StagedClaim.Name, false)
	if err != nil || !matchCorootKeeperStageClaim(claim, corootKeeperStagingClaim(r.Run, r.Source)) ||
		benchmarkIdentity(claim) != r.StagedClaim {
		return retained, errCorootKeeperStaging
	}
	name := "pvc-" + r.StagedClaim.UID
	phase := text(claim, "status", "phase")
	binding := text(claim, "spec", "volumeName")
	if phase == "Pending" && binding == "" {
		// Archive the claim even if the external provisioner never bound it.
		return retained, nil
	}
	if phase != "Bound" || binding != name {
		return retained, errCorootKeeperStaging
	}
	pv, err := j.c.get(ctx, "persistentvolume", "", name, false)
	if err != nil || text(pv, "status", "phase") != "Bound" ||
		!keeperRetainedPVBinding(pv, benchmarkIdentity(pv), r.StagedClaim, "longhorn-v2", "driver.longhorn.io", name) {
		return retained, errCorootKeeperStaging
	}
	volume, err := j.c.get(ctx, "volumes.longhorn.io", "longhorn-system", name, false)
	if err != nil || !recoveredNativeObject(volume, "longhorn.io/v1beta2", "Volume", "longhorn-system") ||
		text(volume, "spec", "dataEngine") != "v2" || text(volume, "spec", "size") != corootKeeperDataBytes {
		return retained, errCorootKeeperStaging
	}
	return corootKeeperRetainedStage{benchmarkIdentity(pv), benchmarkIdentity(volume)}, nil
}

func (j *corootKeeperJournal) observeKeeperStagingBeforeArchive(ctx context.Context, r corootKeeperCutoverRecord) (corootKeeperRetainedStage, error) {
	staging, err := j.observeRetainedKeeperStaging(ctx, r)
	if err != nil || r.StagedClaim.UID == "" || staging != (corootKeeperRetainedStage{}) {
		return staging, err
	}
	// A staging claim can be left Pending when the wait-for-consumer class
	// never got a scheduled consumer. The archive already records its UID.
	if err := verifyLonghornV2RetainClass(ctx, j.c); err != nil {
		return staging, err
	}
	claim, err := j.c.get(ctx, "persistentvolumeclaim", corootKeeperNamespace, r.StagedClaim.Name, false)
	if err != nil || !matchCorootKeeperStageClaim(claim, corootKeeperStagingClaim(r.Run, r.Source)) ||
		benchmarkIdentity(claim) != r.StagedClaim || text(claim, "status", "phase") != "Pending" ||
		text(claim, "spec", "volumeName") != "" ||
		value(claim, "metadata", "annotations", "volume.kubernetes.io/selected-node") != nil ||
		value(claim, "metadata", "annotations", "pv.kubernetes.io/bind-completed") != nil {
		return staging, errCorootKeeperStaging
	}
	name := "pvc-" + r.StagedClaim.UID
	for _, item := range []struct{ kind, namespace string }{
		{"persistentvolume", ""}, {"volumes.longhorn.io", "longhorn-system"},
	} {
		current, err := j.c.get(ctx, item.kind, item.namespace, name, true)
		if err != nil || current != nil {
			return staging, errCorootKeeperStaging
		}
	}
	return staging, j.requireNoClaimConsumers(ctx, r.StagedClaim.Name)
}

func bindCorootKeeperRollback(c client, cm object, input corootKeeperCutoverInput) (corootKeeperJournal, error) {
	journal, err := bindCorootKeeperResumeJournal(c, cm, input)
	if err != nil {
		return corootKeeperJournal{}, err
	}
	r := journal.record
	original := r.Before.Disks[input.Ordinal]
	if (r.Phase != keeperOriginalPreserved && (!mayRestoreOriginalKeeper(r.Phase) || r.Phase == "copied-volumes-detached")) ||
		r.NewClaim != (benchmarkResource{}) || original.Digest != input.Digest ||
		original.Claim.Namespace != corootKeeperNamespace || !isolationUID.MatchString(original.Claim.UID) ||
		original.PV.Namespace != "" || !isolationUID.MatchString(original.PV.UID) ||
		(r.Rebind.Original != (corootKeeperProviderDisk{}) && r.Rebind.Original != original) {
		return corootKeeperJournal{}, errCorootKeeperReceipt
	}
	return journal, nil
}

func rollbackInterruptedCorootKeeper(ctx context.Context, j corootKeeperJournal, source, run string, disks []cnpgRecoveryDiskBinding) error {
	previousRun, previousSource := j.record.executor()
	if run == previousRun || run == j.record.Run {
		return errCorootKeeperReceipt
	}
	conclusion, err := readFinishedKeeperFirstAttempt(ctx, j.c, previousRun, previousSource)
	if err != nil || (conclusion != "failure" && conclusion != "cancelled" && conclusion != "timed_out") {
		return errCorootKeeperReceipt
	}
	next := j.record
	next.ResumeRun, next.ResumeSource = run, source
	if err := j.write(ctx, next); err != nil {
		return err
	}
	if next.Phase != keeperOriginalPreserved {
		if err := j.restoreOriginalBeforeClaimRelease(ctx, disks, true); err != nil {
			return err
		}
	}
	return j.archivePreservedKeeperAttempt(ctx, disks)
}

func readFinishedKeeperFirstAttempt(ctx context.Context, c client, run, source string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	latest, err := readFinishedMigrationRun(ctx, c, run, source)
	if err != nil {
		return "", err
	}
	if latest.Attempt == 1 {
		return latest.Conclusion, nil
	}
	first, err := readFinishedMigrationRunAttempt(ctx, c, run, source, true)
	if err != nil || first.Path != latest.Path {
		return "", errCorootKeeperReceipt
	}
	return first.Conclusion, nil
}

func (j *corootKeeperJournal) verifyRolledBackFencesAbsent(ctx context.Context) error {
	fences := keeperFencesFor(j.record)
	for _, fence := range []object{fences.pod, fences.claim} {
		name := text(fence, "metadata", "name")
		for _, kind := range []string{keeperBindingKind, keeperPolicyKind} {
			active, err := j.c.get(ctx, kind, "", name, true)
			if err != nil || active != nil {
				return errCorootKeeperReceipt
			}
		}
	}
	return nil
}

func (j *corootKeeperJournal) archivePreservedKeeperAttempt(ctx context.Context, disks []cnpgRecoveryDiskBinding) error {
	r := j.record
	if r.Phase != keeperOriginalPreserved {
		return errCorootKeeperReceipt
	}
	if err := j.verifyRolledBackFencesAbsent(ctx); err != nil {
		return err
	}
	current, err := readCorootKeeperQuorum(ctx, j.c, j.source(), disks)
	original := r.Before.Disks[r.Input.Ordinal]
	restored := current.Disks[r.Input.Ordinal]
	if err != nil || current.Controller != r.Before.Controller || current.Budget != r.Before.Budget ||
		restored.Claim != original.Claim || restored.PV != original.PV || restored.Handle != original.Handle ||
		restored.Digest != original.Digest || current.V2Claims[r.Input.Ordinal].UID != "" {
		return errCorootKeeperQuorum
	}
	staging, err := j.observeKeeperStagingBeforeArchive(ctx, r)
	if err != nil || (r.RollbackStaging != (corootKeeperRetainedStage{}) && r.RollbackStaging != staging) {
		return errCorootKeeperStaging
	}
	r.RollbackStaging = staging
	// Preserve the stopped attempt and its retained staged storage identities
	// irrespective of which protected attempt completes the archive.
	r.ResumeRun, r.ResumeSource = "", ""
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	archive := j.identity.Name + "-preserved-" + r.Run
	saved, err := j.c.get(ctx, "configmap", "longhorn-system", archive, true)
	if err != nil {
		return err
	}
	if saved == nil {
		_, err = j.c.create(ctx, j.source(), object{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata": object{"name": archive, "namespace": "longhorn-system"},
			"data":     object{"record": string(encoded), "originalJournalUID": j.identity.UID}})
		if err != nil {
			return err
		}
		saved, err = j.c.get(ctx, "configmap", "longhorn-system", archive, false)
	}
	if err != nil || !recoveredNativeObject(saved, "v1", "ConfigMap", "longhorn-system") ||
		text(saved, "metadata", "name") != archive || value(saved, "immutable") != true ||
		text(saved, "data", "record") != string(encoded) || text(saved, "data", "originalJournalUID") != j.identity.UID {
		return errCorootKeeperReceipt
	}
	journal, err := j.read(ctx)
	if err != nil {
		return err
	}
	confirmed, err := j.observeKeeperStagingBeforeArchive(ctx, r)
	if err != nil || confirmed != staging {
		return errCorootKeeperStaging
	}
	if err := j.c.deleteObserved(ctx, j.source(), "/api/v1/namespaces/longhorn-system/configmaps/"+j.identity.Name, journal); err != nil {
		return err
	}
	return waitReadyWithin(ctx, 30*time.Second, func(ctx context.Context) error {
		current, err := j.c.get(ctx, "configmap", "longhorn-system", j.identity.Name, true)
		if err != nil || (current != nil && benchmarkIdentity(current) != j.identity) {
			return errors.Join(errReadinessTerminal, errCorootKeeperReceipt)
		}
		if current != nil {
			return errCorootKeeperReceipt
		}
		return nil
	})
}
