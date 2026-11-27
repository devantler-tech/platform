package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

const umamiArchiveDataResumeOperation = "resume-umami-archive-data"

type umamiArchiveDataConfirmation struct {
	executor, recordSHA string
}

func authorizeUmamiArchiveDataResume(getenv func(string) string) (umamiArchiveDataConfirmation, error) {
	const operation = umamiArchiveDataResumeOperation
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	rest, prefixed := strings.CutPrefix(confirmation, operation+":")
	previous, fingerprint, separated := strings.Cut(rest, ":")
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	if getenv("LONGHORN_V2_OPERATION") != operation || getenv("LONGHORN_V2_DATABASE") != "umami-db" ||
		getenv("LONGHORN_V2_NODE") != "" || !prefixed || !separated || !seedSHA256.MatchString(fingerprint) ||
		!validBenchmarkOwner(previous, source) || !validBenchmarkOwner(run, source) || previous == run {
		return umamiArchiveDataConfirmation{}, errors.New("the exact retained Umami restore and a distinct first attempt are required")
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return umamiArchiveDataConfirmation{}, err
	}
	return umamiArchiveDataConfirmation{executor: previous, recordSHA: fingerprint}, nil
}

func matchesUmamiArchiveDataCheckpoint(cm object, record cnpgProofRecord, confirmation umamiArchiveDataConfirmation) bool {
	if record.Target != "umami-db" || record.Phase != "cluster-created" || record.Cleaned || record.RestoreVerified ||
		record.RestoreReceipt != nil || record.Plan == nil || record.Credential == nil || record.Preparation == nil ||
		record.Admission == nil || record.Storage == nil || record.Executor == nil || record.Executor.Run != confirmation.executor ||
		record.Executor.JournalUID != text(cm, "metadata", "uid") || len(record.Resources) != 5 {
		return false
	}
	raw := sha256.Sum256([]byte(text(cm, "data", "record")))
	if hex.EncodeToString(raw[:]) != confirmation.recordSHA {
		return false
	}
	ns := "longhorn-db-proof-umami-db-" + record.Run
	seen := map[string]bool{}
	for key, name := range map[string]string{
		"Namespace": ns, "Policy": "restore-isolation", "Secret": "archive-read",
		"ObjectStore": "restore-source", "Cluster": "restore",
	} {
		identity := record.Resources[key]
		namespace := ns
		if key == "Namespace" {
			namespace = ""
		}
		if identity.Name != name || identity.Namespace != namespace || !isolationUID.MatchString(identity.UID) || seen[identity.UID] {
			return false
		}
		seen[identity.UID] = true
	}
	return true
}

func resumeUmamiArchiveData(ctx context.Context, c client) error {
	confirmation, err := authorizeUmamiArchiveDataResume(os.Getenv)
	if err != nil || !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed {
		return errors.New("the protected Umami recovered-data continuation is unverified")
	}
	source, run := os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")
	cm, record, err := readCNPGProof(ctx, c, "umami-db")
	if err != nil || !matchesUmamiArchiveDataCheckpoint(cm, record, confirmation) {
		return errors.New("the confirmed Umami checkpoint has no retained unverified restore")
	}
	previous, err := readFinishedMigrationRun(ctx, c, confirmation.executor, record.Executor.Source)
	if err != nil || previous.Attempt != 1 || previous.Conclusion != "failure" {
		return errors.New("the previous restore executor has no verified first-attempt failure")
	}
	if err := verifyReclaimedNativeRestore(ctx, c, run); err != nil {
		return errors.New("the earlier native V2 proof and scratch retirement are unverified")
	}
	pending := cnpgProofRun{c: c, record: record}
	if err := pending.rebindOriginalArchive(ctx); err != nil {
		return errors.New("the original archive no longer matches the restored checkpoint")
	}
	if _, err := readRecoveredStorageSnapshot(ctx, c, record); err != nil {
		return errors.New("the retained restore and its three V2 copies are unverified")
	}
	if err := pending.verifyDatabaseCreationPolicy(ctx); err != nil {
		return errors.New("the retained restore isolation policy is unverified")
	}
	want := cnpgProofExecutor{Source: source, Run: run, JournalUID: text(cm, "metadata", "uid")}
	if err := qualifyCNPGResumeOwners(ctx, c, record, want); err != nil {
		return err
	}
	p, err := claimCNPGProofExecutor(ctx, c, cm, record, want)
	if err != nil {
		return err
	}
	return certifyCNPGRecoveredArchive(ctx, p)
}
