package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

func bindCorootKeeperRebindResume(c client, cm object, input corootKeeperCutoverInput) (corootKeeperJournal, error) {
	journal, err := bindCorootKeeperResumeJournal(c, cm, input)
	if err != nil {
		return corootKeeperJournal{}, err
	}
	record := journal.record
	if record.Rebind.Original != record.Before.Disks[input.Ordinal] || record.Rebind.Original.Digest != input.Digest ||
		record.Rebind.Coroot != record.Coroot || record.Rebind.StagedClaim != record.StagedClaim ||
		!isolationUID.MatchString(record.Rebind.StagedPV.UID) || !isolationUID.MatchString(record.Rebind.StagedVolume.UID) ||
		!isolationUID.MatchString(record.PodFence.PolicyUID) || !isolationUID.MatchString(record.PodFence.BindingUID) ||
		!isolationUID.MatchString(record.ClaimFence.PolicyUID) || !isolationUID.MatchString(record.ClaimFence.BindingUID) {
		return corootKeeperJournal{}, errCorootKeeperReceipt
	}
	phase := slices.Index(keeperCutoverPhases, record.Phase)
	if phase < slices.Index(keeperCutoverPhases, "copied-volumes-detached") {
		return corootKeeperJournal{}, errors.New("the recorded Keeper migration has no verified detached copy")
	}
	return journal, nil
}

func bindCorootKeeperResumeJournal(c client, cm object, input corootKeeperCutoverInput) (corootKeeperJournal, error) {
	var record corootKeeperCutoverRecord
	raw := text(cm, "data", "record")
	if input.Ordinal < 0 || input.Ordinal > 2 || !retentionDigest.MatchString(input.Digest) ||
		!recoveredNativeObject(cm, "v1", "ConfigMap", "longhorn-system") ||
		text(cm, "metadata", "name") != corootKeeperJournalName(input.Ordinal) || len(raw) > 65536 ||
		json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 || record.Input != input ||
		!validCorootKeeperFenceUser(record.FenceUser) || !validBenchmarkOwner(record.Run, record.Source) ||
		text(cm, "metadata", "annotations", corootKeeperFenceOwner) != record.Run+":"+record.Source {
		return corootKeeperJournal{}, errCorootKeeperReceipt
	}
	run, source := record.executor()
	if !validBenchmarkOwner(run, source) || (record.ResumeRun == "") != (record.ResumeSource == "") {
		return corootKeeperJournal{}, errCorootKeeperReceipt
	}
	encoded, err := json.Marshal(record)
	if err != nil || string(encoded) != raw {
		return corootKeeperJournal{}, errCorootKeeperReceipt
	}
	return corootKeeperJournal{c: c, identity: benchmarkIdentity(cm), record: record}, nil
}

func resumeCorootKeeperRebind(ctx context.Context, c client, source, run string, input corootKeeperCutoverInput, cm object, disks []cnpgRecoveryDiskBinding) error {
	journal, err := bindCorootKeeperRebindResume(c, cm, input)
	if err != nil {
		return err
	}
	prior, priorSource := journal.record.executor()
	if run == prior || run == journal.record.Run {
		return errCorootKeeperReceipt
	}
	finished, err := readFinishedMigrationRun(ctx, c, prior, priorSource)
	if err != nil || finished.Attempt != 1 {
		return errCorootKeeperReceipt
	}
	if journal.record.Phase == "replacement-verified" {
		return journal.verifyRejoined(ctx, journal.record, disks, source)
	}
	if finished.Conclusion != "failure" && finished.Conclusion != "cancelled" && finished.Conclusion != "timed_out" {
		return errCorootKeeperReceipt
	}
	next := journal.record
	next.ResumeRun, next.ResumeSource = run, source
	if err := journal.write(ctx, next); err != nil {
		return err
	}
	return journal.rebindAndResume(ctx, keeperFencesFor(next), disks)
}
