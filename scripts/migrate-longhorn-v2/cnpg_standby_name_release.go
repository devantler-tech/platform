package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

const (
	recoverUmamiStandbyNameOperation = "recover-umami-standby-name"
	umamiNameFencePending            = "name-fence-requested"
	umamiNameFenced                  = "name-fenced"
	umamiJoinDeletePending           = "stale-join-release-requested"
	umamiClaimDeletePending          = "claim-name-release-requested"
	umamiNameOriginalPreserved       = "claim-name-released"
)

type umamiStandbyNameRelease struct {
	Owner    umamiReplacementOwner
	Join     benchmarkResource
	Fence    benchmarkFenceReceipt
	Executor *umamiReplacementOwner `json:",omitempty"`
}

func (r umamiStandbyNameRelease) executor() umamiReplacementOwner {
	if r.Executor != nil {
		return *r.Executor
	}
	return r.Owner
}

type umamiStandbyNameRecovery struct {
	original          umamiStandbyResume
	eviction          umamiReplacementOwner
	joinUID, previous string
}

func authorizeUmamiStandbyNameRecovery(getenv func(string) string) (umamiStandbyNameRecovery, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	parts := strings.Split(confirmation, ":")
	refused := errors.New("name recovery requires the exact Umami journal, terminal eviction and completed join job")
	if (len(parts) != 8 && len(parts) != 9) || parts[0] != recoverUmamiStandbyNameOperation ||
		getenv("LONGHORN_V2_OPERATION") != recoverUmamiStandbyNameOperation ||
		getenv("LONGHORN_V2_DATABASE") != "umami-db" || getenv("LONGHORN_V2_NODE") != "" ||
		!validBenchmarkOwner(parts[2], parts[3]) || !isolationUID.MatchString(parts[4]) ||
		!validBenchmarkOwner(parts[5], parts[6]) || !isolationUID.MatchString(parts[7]) {
		return umamiStandbyNameRecovery{}, refused
	}
	if _, err := cnpgInstanceID(parts[1], "umami-db"); err != nil {
		return umamiStandbyNameRecovery{}, refused
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return umamiStandbyNameRecovery{}, err
	}
	previous := ""
	if len(parts) == 9 {
		previous = parts[8]
		if !benchmarkRunPattern.MatchString(previous) || previous == getenv("GITHUB_RUN_ID") {
			return umamiStandbyNameRecovery{}, refused
		}
	}
	return umamiStandbyNameRecovery{
		original: umamiStandbyResume{target: parts[1], run: parts[2], source: parts[3], uid: parts[4]},
		eviction: umamiReplacementOwner{Run: parts[5], Source: parts[6]}, joinUID: parts[7], previous: previous,
	}, nil
}

func bindUmamiNameRecovery(cm object, input umamiStandbyNameRecovery, owner umamiReplacementOwner) (umamiStandbyReplacementRecord, error) {
	prior := input.original
	raw := text(cm, "data", "record")
	var record umamiStandbyReplacementRecord
	refused := errors.New("the exact interrupted Umami eviction journal is unverified")
	if len(raw) > 65536 || json.Unmarshal([]byte(raw), &record) != nil ||
		!validUmamiOriginalReplacement(record) || record.Original.Consumer.Name != prior.target ||
		record.Run != prior.run || record.Source != prior.source || record.Replacement != nil ||
		!validBenchmarkOwner(owner.Run, owner.Source) || owner.Run == prior.run || owner.Run == input.eviction.Run ||
		owner.Source == prior.source || owner.Source == input.eviction.Source {
		return record, refused
	}
	eviction := umamiReplacementOwner{Run: record.Run, Source: record.Source}
	if record.Continuation != nil {
		eviction = *record.Continuation
	}
	if eviction != input.eviction {
		return record, refused
	}
	if input.previous == "" {
		if record.Phase != umamiReplacementEvictionPending || record.NameRelease != nil {
			return record, refused
		}
	} else if !umamiNameReleaseCanResume(record) || record.NameRelease.Join.UID != input.joinUID ||
		record.NameRelease.executor().Run != input.previous || owner.Run == input.previous {
		return record, refused
	}
	return record, nil
}

func umamiNameReleaseCanResume(record umamiStandbyReplacementRecord) bool {
	if record.NameRelease == nil {
		return false
	}
	switch record.Phase {
	case umamiNameFencePending, umamiNameFenced, umamiJoinDeletePending, umamiClaimDeletePending, umamiNameOriginalPreserved, umamiReplacementOriginalDetached:
		return true
	default:
		return false
	}
}

func validUmamiNameReleaseProgress(previous, next umamiStandbyReplacementRecord) bool {
	release := next.NameRelease
	name := next.Original.Consumer.Name
	if release == nil || !validBenchmarkOwner(release.Owner.Run, release.Owner.Source) ||
		release.Owner.Run == next.Run || release.Owner.Source == next.Source ||
		(next.Continuation != nil && (release.Owner.Run == next.Continuation.Run || release.Owner.Source == next.Continuation.Source)) ||
		release.Join.Namespace != "umami" || release.Join.Name != name+"-join" ||
		!isolationUID.MatchString(release.Join.UID) {
		return false
	}
	if previous.Phase == umamiReplacementEvictionPending {
		return next.Phase == umamiNameFencePending && previous.NameRelease == nil && release.Executor == nil && release.Fence == (benchmarkFenceReceipt{})
	}
	before := previous.NameRelease
	if before == nil || before.Owner != release.Owner || before.Join != release.Join {
		return false
	}
	if next.Phase == previous.Phase {
		if previous.Phase == umamiNameFencePending && before.Fence == (benchmarkFenceReceipt{}) &&
			isolationUID.MatchString(release.Fence.PolicyUID) && release.Fence.BindingUID == "" &&
			reflect.DeepEqual(before.Executor, release.Executor) {
			return true
		}
		executor := release.executor()
		return umamiNameReleaseCanResume(previous) && release.Executor != nil &&
			validBenchmarkOwner(executor.Run, executor.Source) && executor.Run != before.executor().Run &&
			executor.Run != before.Owner.Run && executor.Run != previous.Run &&
			(previous.Continuation == nil || executor.Run != previous.Continuation.Run) &&
			before.Fence == release.Fence
	}
	if !reflect.DeepEqual(before.Executor, release.Executor) {
		return false
	}
	switch previous.Phase {
	case umamiNameFencePending:
		return next.Phase == umamiNameFenced && isolationUID.MatchString(release.Fence.PolicyUID) &&
			before.Fence.PolicyUID == release.Fence.PolicyUID && before.Fence.BindingUID == "" &&
			isolationUID.MatchString(release.Fence.BindingUID) && release.Fence.PolicyUID != release.Fence.BindingUID
	case umamiNameFenced:
		return next.Phase == umamiJoinDeletePending && before.Fence == release.Fence
	case umamiJoinDeletePending:
		return next.Phase == umamiClaimDeletePending && before.Fence == release.Fence
	case umamiClaimDeletePending:
		return next.Phase == umamiNameOriginalPreserved && before.Fence == release.Fence
	case umamiNameOriginalPreserved:
		return next.Phase == umamiReplacementOriginalDetached && before.Fence == release.Fence
	default:
		return false
	}
}

func runUmamiNameRecovery(ctx context.Context, c client, input umamiStandbyNameRecovery, owner umamiReplacementOwner) error {
	if c.context != "admin@prod" {
		return errors.New("the Umami name recovery requires the protected production workflow")
	}
	cm, err := c.get(ctx, "configmap", umamiStandbyJournalNamespace, "longhorn-v2-standby-replacement-"+input.original.target, false)
	if err != nil {
		return err
	}
	record, err := bindUmamiNameRecovery(cm, input, owner)
	if err != nil {
		return err
	}
	journal := umamiStandbyReplacementJournal{c: c, uid: input.original.uid, record: record, resume: &owner}
	if _, err = journal.read(ctx); err != nil {
		return err
	}
	for _, prior := range []umamiReplacementOwner{{Run: input.original.run, Source: input.original.source}, input.eviction} {
		run, readErr := readFinishedMigrationRun(ctx, c, prior.Run, prior.Source)
		if readErr != nil || run.Attempt != 1 || run.Conclusion != "failure" {
			return errors.New("previous Umami eviction workflow is not a verified terminal attempt")
		}
	}
	if record.Phase != umamiNameOriginalPreserved && record.Phase != umamiReplacementOriginalDetached {
		if err = verifyUmamiNameReleaseArchive(ctx, &journal); err != nil {
			return err
		}
	}
	if record.NameRelease != nil {
		prior := record.NameRelease.executor()
		run, readErr := readFinishedMigrationRun(ctx, c, prior.Run, prior.Source)
		if readErr != nil || run.Attempt != 1 || run.Conclusion == "success" {
			return errors.New("the interrupted name recovery is not a verified terminal attempt")
		}
		next := journal.record
		release := *next.NameRelease
		release.Executor = &owner
		next.NameRelease = &release
		if err = journal.transition(ctx, next); err != nil {
			return err
		}
		return finishUmamiNameRecovery(ctx, &journal)
	}
	original := journal.record.Original
	join := benchmarkResource{Name: original.Consumer.Name + "-join", Namespace: original.Consumer.Namespace, UID: input.joinUID}
	if _, _, err = readUmamiNameReleaseState(ctx, &journal, join, true); err != nil {
		return err
	}
	next := journal.record
	next.Phase = umamiNameFencePending
	next.NameRelease = &umamiStandbyNameRelease{Owner: owner, Join: join}
	if err = journal.transition(ctx, next); err != nil {
		return err
	}
	return finishUmamiNameRecovery(ctx, &journal)
}

func advanceUmamiNameRelease(ctx context.Context, j *umamiStandbyReplacementJournal, phase string) error {
	next := j.record
	next.Phase = phase
	return j.transition(ctx, next)
}

func finishUmamiNameRecovery(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	release := j.record.NameRelease
	if release == nil || release.executor() != j.executor() {
		return errors.New("the Umami claim name release has no verified workflow owner")
	}
	if j.record.Phase == umamiNameFencePending {
		if _, _, err := readUmamiNameReleaseState(ctx, j, release.Join, true); err != nil {
			return err
		}
		if err := installUmamiNameFence(ctx, j); err != nil {
			return err
		}
	}
	if j.record.Phase == umamiNameFenced {
		if err := advanceUmamiNameRelease(ctx, j, umamiJoinDeletePending); err != nil {
			return err
		}
	}
	if j.record.Phase == umamiJoinDeletePending {
		if err := releaseUmamiStaleJoin(ctx, j); err != nil {
			return err
		}
		if err := advanceUmamiNameRelease(ctx, j, umamiClaimDeletePending); err != nil {
			return err
		}
	}
	if j.record.Phase == umamiClaimDeletePending {
		if err := releaseUmamiOriginalClaimName(ctx, j); err != nil {
			return err
		}
		if err := advanceUmamiNameRelease(ctx, j, umamiNameOriginalPreserved); err != nil {
			return err
		}
	}
	if j.record.Phase == umamiNameOriginalPreserved {
		if err := removeUmamiNameFence(ctx, j); err != nil {
			return err
		}
		if err := advanceUmamiNameRelease(ctx, j, umamiReplacementOriginalDetached); err != nil {
			return err
		}
	}
	return observeUmamiReplacementReady(ctx, j)
}

func releaseUmamiStaleJoin(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	c, original, release := j.c, j.record.Original, j.record.NameRelease
	if err := proveUmamiNameFence(ctx, j); err != nil {
		return err
	}
	job, err := c.get(ctx, "jobs.batch", release.Join.Namespace, release.Join.Name, true)
	if err != nil {
		return err
	}
	if job != nil {
		if benchmarkIdentity(job) != release.Join {
			return errors.New("the witnessed Umami join job was replaced")
		}
		if value(job, "metadata", "deletionTimestamp") == nil {
			observed, _, readErr := readUmamiNameReleaseState(ctx, j, release.Join, true)
			if readErr != nil {
				return readErr
			}
			path := "/apis/batch/v1/namespaces/" + original.Consumer.Namespace + "/jobs/" + release.Join.Name
			if err = c.deleteObserved(ctx, j.executor().Source, path, observed); err != nil {
				return err
			}
		}
	}
	return waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		_, _, readErr := readUmamiNameReleaseState(ctx, j, release.Join, false)
		return readErr
	})
}

func releaseUmamiOriginalClaimName(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	c, original, release := j.c, j.record.Original, j.record.NameRelease
	if err := verifyUmamiNameReleaseArchive(ctx, j); err != nil {
		return err
	}
	if err := proveUmamiNameFence(ctx, j); err != nil {
		return err
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", original.Claim.Namespace, original.Claim.Name, true)
	if err != nil {
		return err
	}
	if claim != nil {
		if benchmarkIdentity(claim) != original.Claim {
			return errors.New("the witnessed Umami claim was replaced before releasing its name")
		}
		if value(claim, "metadata", "deletionTimestamp") == nil {
			_, observed, readErr := readUmamiNameReleaseState(ctx, j, release.Join, false)
			if readErr != nil {
				return readErr
			}
			path := "/api/v1/namespaces/" + original.Claim.Namespace + "/persistentvolumeclaims/" + original.Claim.Name
			if err = c.deleteObserved(ctx, j.executor().Source, path, observed); err != nil {
				return err
			}
		}
	}
	return waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		return observeUmamiReleasedOriginal(ctx, j, true)
	})
}
