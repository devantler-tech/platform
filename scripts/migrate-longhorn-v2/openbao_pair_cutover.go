package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func runOpenBaoPairCutover(ctx context.Context, c client, input openBaoPairInput, prior, source, run, token, release string) error {
	if c.context != "admin@prod" || token == "" || !openBaoReleaseVersion.MatchString(release) {
		return errOpenBaoProviderStorage
	}
	plan, err := observeOpenBaoPairPlan(ctx, c, input.Member, run, source)
	if err != nil || plan.Input != input {
		return errOpenBaoProviderStorage
	}
	proveFollower := func(ctx context.Context, member benchmarkResource) error {
		return verifyOpenBaoRaftMember(ctx, c, member, release, true)
	}
	if err := proveFollower(ctx, plan.Original.Member); err != nil {
		return err
	}
	preflight := openBaoPairJournal{client: c, record: openBaoPairRecord{Plan: plan}}
	if _, err := preflight.readEvictionSource(ctx, token); err != nil {
		return err
	}
	if err := verifyLonghornV2RetainClass(ctx, c); err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	var previous openBaoPairRecord
	var expectedArchive string
	var reuse [3]benchmarkResource
	var requestedSize *string
	if prior == "" {
		size := "50Gi"
		requestedSize = &size
	} else {
		previous, expectedArchive, err = observeReusableOpenBaoStage(ctx, c, plan, prior, token)
		if err != nil {
			return fmt.Errorf("OpenBao retained staging proof: %w", err)
		}
		plan, reuse, err = bindOpenBaoStageReuse(previous, plan, prior)
		if err != nil {
			return err
		}
	}
	capacity, err := activation.observeRecoveryStorage(ctx, requestedSize)
	if err != nil {
		return err
	}
	if err := archiveRestoredOpenBaoAttempt(ctx, c, plan, token, expectedArchive); err != nil {
		return err
	}
	if prior != "" {
		if err := writeRestoredOpenBaoArchive(ctx, c, source, previous, expectedArchive); err != nil {
			return err
		}
	}
	j, err := beginOpenBaoPairJournal(ctx, c, plan, reuse)
	if err != nil {
		return err
	}
	heldBeforeRelease := func(step string, failure error) error {
		verdict := fmt.Errorf("OpenBao migration held at %s: %w", step, failure)
		if ctx.Err() != nil {
			return verdict
		}
		recovery, cancel := context.WithTimeout(ctx, 9*time.Minute)
		defer cancel()
		if err := j.resumeOriginalPair(recovery, token, release); err != nil {
			return errors.Join(verdict, fmt.Errorf("original member recovery is unverified: %w", err))
		}
		return errors.Join(verdict, errors.New("the original member and both retained claims are serving"))
	}
	if err := j.stageDestinations(ctx, capacity.Disks, previous.Destinations); err != nil {
		return heldBeforeRelease("destination staging", err)
	}
	if err := j.installAdmissionFences(ctx); err != nil {
		return heldBeforeRelease("admission fences", err)
	}
	if err := j.requestFollowerEviction(ctx, token, proveFollower); err != nil {
		return heldBeforeRelease("follower eviction", err)
	}
	if err := j.copyStagedPair(ctx, token, capacity.Disks); err != nil {
		return heldBeforeRelease("offline copy", err)
	}
	if err := j.releaseCopiedClaims(ctx, token); err != nil {
		if j.record.Phase == openBaoPairCopied {
			return heldBeforeRelease("claim release preflight", err)
		}
		return fmt.Errorf("OpenBao claim release is unverified; preserve the journal and all retained PVs: %w", err)
	}
	if err := j.rebindCopiedClaims(ctx, token); err != nil {
		return fmt.Errorf("OpenBao pair binding is unverified; preserve the journal and all retained PVs: %w", err)
	}
	return j.resumeWithV2Pair(ctx, token, release, capacity.Disks)
}
