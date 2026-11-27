package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const keeperOriginalPreserved = "original-claim-preserved"

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
	finished, err := readFinishedMigrationRun(ctx, j.c, previousRun, previousSource)
	if err != nil || finished.Attempt != 1 ||
		(finished.Conclusion != "failure" && finished.Conclusion != "cancelled" && finished.Conclusion != "timed_out") {
		return errCorootKeeperReceipt
	}
	next := j.record
	next.ResumeRun, next.ResumeSource = run, source
	if err := j.write(ctx, next); err != nil {
		return err
	}
	if next.Phase != keeperOriginalPreserved {
		if err := j.restoreOriginalBeforeClaimRelease(ctx, disks); err != nil {
			return err
		}
	}
	return j.archivePreservedKeeperAttempt(ctx, disks)
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
	if err != nil || current.Controller != r.Before.Controller || current.Budget != r.Before.Budget ||
		current.Disks[r.Input.Ordinal] != r.Before.Disks[r.Input.Ordinal] || current.V2Claims[r.Input.Ordinal].UID != "" {
		return errCorootKeeperQuorum
	}
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
