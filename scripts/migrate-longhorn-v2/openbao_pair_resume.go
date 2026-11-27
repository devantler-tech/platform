package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

const openBaoPairResumeOperation = "resume-openbao-v2-pair"

func authorizeOpenBaoPairResume(getenv func(string) string) (openBaoPairInput, string, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	fields := strings.Split(confirmation, ":")
	if getenv("LONGHORN_V2_OPERATION") != openBaoPairResumeOperation || getenv("LONGHORN_V2_NODE") != "" ||
		getenv("LONGHORN_V2_DATABASE") != "" || len(fields) != 4 || fields[0] != openBaoPairResumeOperation ||
		!knownOpenBaoMember(fields[1]) || !retentionDigest.MatchString(fields[2]) ||
		!validBenchmarkOwner(fields[3], getenv("GITHUB_SHA")) || fields[3] == getenv("GITHUB_RUN_ID") {
		return openBaoPairInput{}, "", errOpenBaoProviderStorage
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return openBaoPairInput{}, "", err
	}
	return openBaoPairInput{Member: fields[1], Digest: fields[2]}, fields[3], nil
}

func runOpenBaoPairResume(ctx context.Context, c client, input openBaoPairInput, priorRun, source, run, token, release string) (bool, error) {
	if c.context != "admin@prod" || token == "" || !openBaoReleaseVersion.MatchString(release) ||
		!validBenchmarkOwner(priorRun, source) || !validBenchmarkOwner(run, source) || priorRun == run {
		return false, errOpenBaoProviderStorage
	}
	if err := verifyInterruptedOpenBaoAttempt(ctx, c, priorRun, source); err != nil {
		return false, err
	}
	cm, err := c.get(ctx, "configmap", openBaoPairJournalNamespace, openBaoPairJournalName(input.Member), false)
	var record openBaoPairRecord
	if err != nil || json.Unmarshal([]byte(text(cm, "data", "record")), &record) != nil ||
		!record.valid() || record.RolledBack || record.Plan.Input != input ||
		record.Plan.Run != priorRun || record.Plan.Source != source || record.FencePending != "" || record.CopyPending != "" {
		return false, errOpenBaoProviderStorage
	}
	j := openBaoPairJournal{client: c, id: benchmarkIdentity(cm), record: record}
	if _, err := j.read(ctx); err != nil {
		return false, err
	}
	if record.FenceUser != "" {
		user, err := corootKeeperFenceUser(ctx, c)
		if err != nil || user != record.FenceUser {
			return false, errOpenBaoProviderStorage
		}
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	storage, err := activation.observeRecoveryStorage(ctx, nil)
	if err != nil {
		return false, err
	}
	switch j.record.Phase {
	case openBaoPairPlanned, openBaoPairStaged, openBaoPairCopied:
		return false, j.resumeOriginalPair(ctx, token, release)
	case openBaoPairReleasing:
		if err := j.releaseCopiedClaims(ctx, token); err != nil {
			return false, err
		}
	}
	if j.record.Phase == openBaoPairSourcesReleased {
		if err := j.rebindCopiedClaims(ctx, token); err != nil {
			return false, err
		}
	}
	if err := j.resumeWithV2Pair(ctx, token, release, storage.Disks); err != nil {
		return false, err
	}
	return true, nil
}

func verifyInterruptedOpenBaoAttempt(ctx context.Context, c client, run, source string) error {
	if !validBenchmarkOwner(run, source) {
		return errOpenBaoProviderStorage
	}
	runID, err := strconv.ParseUint(run, 10, 53)
	if err != nil {
		return errOpenBaoProviderStorage
	}
	raw, err := c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+run+"/attempts/1")
	var attempt object
	if err != nil || json.Unmarshal(raw, &attempt) != nil || text(attempt, "status") != "completed" ||
		text(attempt, "head_sha") != source || text(attempt, "head_branch") != "main" || text(attempt, "event") != "workflow_dispatch" ||
		text(attempt, "path") != ".github/workflows/prepare-longhorn-v2-node.yaml" ||
		value(attempt, "id") != float64(runID) || value(attempt, "run_attempt") != float64(1) ||
		value(attempt, "workflow_id") != float64(401122310) ||
		(text(attempt, "conclusion") != "failure" && text(attempt, "conclusion") != "cancelled") {
		return errOpenBaoProviderStorage
	}
	return nil
}
