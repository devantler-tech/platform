package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const resumeUmamiStandbyOperation = "resume-retained-umami-v2-standby"

type umamiStandbyResume struct {
	target, run, source, uid string
}

func authorizeUmamiStandbyResume(getenv func(string) string) (umamiStandbyResume, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	parts := strings.Split(confirmation, ":")
	refused := errors.New("the exact retained Umami standby, original run, source and journal UID are required")
	if len(parts) != 5 || parts[0] != resumeUmamiStandbyOperation ||
		getenv("LONGHORN_V2_OPERATION") != resumeUmamiStandbyOperation ||
		getenv("LONGHORN_V2_DATABASE") != "umami-db" || getenv("LONGHORN_V2_NODE") != "" ||
		!validBenchmarkOwner(parts[2], parts[3]) || !isolationUID.MatchString(parts[4]) {
		return umamiStandbyResume{}, refused
	}
	if _, err := cnpgInstanceID(parts[1], "umami-db"); err != nil {
		return umamiStandbyResume{}, refused
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return umamiStandbyResume{}, err
	}
	return umamiStandbyResume{target: parts[1], run: parts[2], source: parts[3], uid: parts[4]}, nil
}

func resumeUmamiStandbyReplacement(ctx context.Context, c client, next umamiReplacementOwner, prior umamiStandbyResume) error {
	journal, err := readResumableUmamiStandby(ctx, c, next, prior)
	if err != nil {
		return err
	}
	if err = evictUmamiRetainedStandby(ctx, &journal); err != nil {
		return err
	}
	return observeUmamiStandbyRebuild(ctx, &journal)
}

// Claim-retained is the last checkpoint before eviction intent, so a request
// from the terminal original run cannot be in flight.
func readResumableUmamiStandby(ctx context.Context, c client, next umamiReplacementOwner, prior umamiStandbyResume) (umamiStandbyReplacementJournal, error) {
	refused := errors.New("the exact interrupted Umami standby retention is unverified")
	_, targetErr := cnpgInstanceID(prior.target, "umami-db")
	if c.context != "admin@prod" || targetErr != nil || !validBenchmarkOwner(prior.run, prior.source) ||
		!isolationUID.MatchString(prior.uid) || !validBenchmarkOwner(next.Run, next.Source) ||
		next.Run == prior.run || next.Source == prior.source {
		return umamiStandbyReplacementJournal{}, refused
	}
	name := "longhorn-v2-standby-replacement-" + prior.target
	cm, err := c.get(ctx, "configmap", umamiStandbyJournalNamespace, name, false)
	if err != nil {
		return umamiStandbyReplacementJournal{}, err
	}
	record, err := bindRetainedUmamiStandby(cm, name, prior)
	if err != nil {
		return umamiStandbyReplacementJournal{}, err
	}
	run, err := readFinishedMigrationRun(ctx, c, prior.run, prior.source)
	if err != nil || run.Attempt != 1 || run.Conclusion != "failure" {
		return umamiStandbyReplacementJournal{}, refused
	}
	if err = sourceCurrent(ctx, c.run, next.Source); err != nil {
		return umamiStandbyReplacementJournal{}, err
	}
	journal := umamiStandbyReplacementJournal{c: c, uid: prior.uid, record: record, resume: &next}
	if _, err = journal.read(ctx); err != nil {
		return umamiStandbyReplacementJournal{}, err
	}
	return journal, nil
}

func bindRetainedUmamiStandby(cm object, name string, prior umamiStandbyResume) (umamiStandbyReplacementRecord, error) {
	refused := errors.New("the original Umami journal does not record a single retained claim awaiting eviction")
	raw := text(cm, "data", "record")
	var record umamiStandbyReplacementRecord
	if !recoveredNativeObject(cm, "v1", "ConfigMap", umamiStandbyJournalNamespace) ||
		text(cm, "metadata", "name") != name || text(cm, "metadata", "uid") != prior.uid ||
		text(cm, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" ||
		text(cm, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != prior.run ||
		len(raw) > 65536 || json.Unmarshal([]byte(raw), &record) != nil ||
		record.Run != prior.run || record.Source != prior.source || record.Original.Consumer.Name != prior.target ||
		record.Continuation != nil || record.Replacement != nil || record.NameRelease != nil || record.Phase != umamiReplacementClaimRetained ||
		!validUmamiOriginalReplacement(record) {
		return umamiStandbyReplacementRecord{}, refused
	}
	encoded, err := json.Marshal(record)
	if err != nil || string(encoded) != raw {
		return umamiStandbyReplacementRecord{}, refused
	}
	return record, nil
}
