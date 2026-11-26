package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

const umamiArchiveIsolationResumeOperation = "resume-umami-archive-isolation"

func authorizeUmamiArchiveIsolationResume(getenv func(string) string) (string, error) {
	const operation = umamiArchiveIsolationResumeOperation
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	prefix := operation + ":"
	previous := strings.TrimPrefix(confirmation, prefix)
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	refused := errors.New("the original Umami isolation checkpoint and a distinct first attempt are required")
	if getenv("LONGHORN_V2_OPERATION") != operation || getenv("LONGHORN_V2_DATABASE") != "umami-db" ||
		getenv("LONGHORN_V2_NODE") != "" || !strings.HasPrefix(confirmation, prefix) ||
		!validBenchmarkOwner(previous, source) || !validBenchmarkOwner(run, source) || previous == run {
		return "", refused
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return "", err
	}
	return previous, nil
}

func resumeUmamiArchiveIsolation(ctx context.Context, c client) error {
	previous, err := authorizeUmamiArchiveIsolationResume(os.Getenv)
	if err != nil || !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed {
		return errors.New("the protected Umami archive continuation is unverified")
	}
	const target = "umami-db"
	source, run := os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")
	if err := verifyReclaimedNativeRestore(ctx, c, run); err != nil {
		return errors.New("database archive continuation requires the verified V2 restore and retired test volume")
	}
	cm, record, err := readCNPGProof(ctx, c, target)
	if err != nil || record.Run != previous || record.Phase != "policy-created" || !cnpgResumeCheckpointEligible(record) {
		return errors.New("the confirmed original proof is outside its empty isolation checkpoint")
	}
	initial, err := readFinishedMigrationRun(ctx, c, previous, record.Source)
	if err != nil || initial.Attempt != 1 || initial.Conclusion != "failure" {
		return errors.New("the original isolated recovery run has no verified first-attempt failure")
	}
	p, err := claimCNPGProofResume(ctx, c, cm, record, source, run)
	if err != nil {
		return fmt.Errorf("archive continuation held at the retained Umami isolation checkpoint: %w", err)
	}
	if err := p.prepareIsolation(ctx); err != nil {
		return fmt.Errorf("archive continuation held at native isolation import: %w", err)
	}
	return finishCNPGArchiveProof(ctx, p)
}
