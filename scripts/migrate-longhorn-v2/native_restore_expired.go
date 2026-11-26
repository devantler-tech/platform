package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"time"
)

func verifyExpiredNativeRestoreTransfer(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, restore, pod object, transfers, repositories []object) (nativeRestoreTransferReceipt, error) {
	if state.Phase != "restore-owned" || !nativeRestorePodExpired(pod) {
		return nativeRestoreTransferReceipt{}, errNativeRestoreEvidence
	}
	return verifyNativeRestoreTransferData(record, seed, state, restore, pod, transfers, repositories)
}

// Only the known bounded canary's terminal deadline state admits replacement.
// Evicted, restarted and unknown containers need separate recovery.
func nativeRestorePodExpired(pod object) bool {
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if text(pod, "status", "phase") != "Failed" || text(pod, "status", "reason") != "DeadlineExceeded" ||
		ready(valueObject(pod, "status")) || value(pod, "metadata", "deletionTimestamp") != nil ||
		value(pod, "spec", "activeDeadlineSeconds") != float64(2700) || text(pod, "spec", "restartPolicy") != "Never" || len(statuses) != 1 {
		return false
	}
	process, _ := statuses[0].(object)
	termination := valueObject(process, "state", "terminated")
	start, startErr := time.Parse(time.RFC3339Nano, text(pod, "status", "startTime"))
	exited, exitErr := time.Parse(time.RFC3339Nano, text(termination, "finishedAt"))
	running, runningErr := time.Parse(time.RFC3339Nano, text(termination, "startedAt"))
	return text(process, "name") == "fio" && value(process, "ready") == false && value(process, "restartCount") == float64(0) &&
		len(valueObject(process, "state")) == 1 && (value(termination, "exitCode") == float64(137) || value(termination, "exitCode") == float64(143)) &&
		startErr == nil && exitErr == nil && runningErr == nil && !running.Before(start) && !exited.Before(running) &&
		!exited.Before(start.Add(45*time.Minute)) && !exited.After(time.Now())
}

// Continue on the original detached volume. The expired Pod remains the
// Velero transfer source in the journal. Its replacement is the first process
// permitted to inspect the bytes and participate in the reattachment check.
func (r *nativeRestoreRun) resumeExpired(ctx context.Context, expired object, executor string, velero nativeRestoreVeleroEpoch) error {
	source := r.plan.Seed
	restore, transfers, repositories, err := r.readRestoreTransferObjects(ctx)
	if err != nil {
		return err
	}
	witness, err := verifyExpiredNativeRestoreTransfer(source.Record, source.Seed, r.state, restore, expired, transfers, repositories)
	if err != nil || verifyNativeCanaryInit(expired, &nativeRestoreInitProof{
		RestoreUID: r.state.Resources["Restore"].UID, ImageDigest: velero.Digest,
	}) != nil {
		return errors.New("the expired original consumer and completed Velero transfer could not be rebound")
	}
	expiration := nativeRestoreExpiredReceipt{Pod: witness.Pod, Transfer: witness.Transfer}
	planned := r.state
	planned.Phase, planned.Expired = nativeExpiredDeleteRequested, &expiration
	detached, err := readNativeRestoreDetached(ctx, r.c, planned)
	if err != nil || verifyNativeRestoreExpiredDetached(source.Record, source.Seed, planned, detached) != nil {
		return errors.New("the original V2 claim must be detached before replacing the expired consumer")
	}
	if err := r.verifyExpiredRestorePolicy(ctx); err != nil {
		return err
	}
	if err := r.claimResume(ctx, executor); err != nil {
		return err
	}
	if err := r.advance(ctx, nativeExpiredDeleteRequested, func(next *nativeRestoreState) { next.Expired = &expiration }); err != nil {
		return err
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.source, "/api/v1/namespaces/"+benchmarkNamespace+"/pods/fio", expired); err != nil {
		return errors.New("the original expired consumer deletion could not be confirmed")
	}
	if err := waitReady(ctx, r.observeDetached); err != nil {
		return err
	}
	if err := r.advance(ctx, nativeExpiredPodDeleted, nil); err != nil {
		return err
	}
	if err := r.createExpiredReplacement(ctx, witness.Node); err != nil {
		return err
	}
	if err := r.verifyExpiredReplacement(ctx, witness.Node, velero); err != nil {
		return err
	}
	return r.verifyReattachment(ctx, velero)
}

func (r *nativeRestoreRun) verifyExpiredRestorePolicy(ctx context.Context) error {
	id := r.state.Resources["NetworkPolicy"]
	policy, err := r.c.get(ctx, "networkpolicies", id.Namespace, id.Name, false)
	if err != nil || benchmarkIdentity(policy) != id || value(policy, "metadata", "deletionTimestamp") != nil {
		return errNativeRestoreEvidence
	}
	assets, err := benchmarkResources("v2", r.state.Run, benchmarkNamespace)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if text(asset, "kind") == "NetworkPolicy" && reflect.DeepEqual(policy["spec"], asset["spec"]) {
			return nil
		}
	}
	return errNativeRestoreEvidence
}

func (r *nativeRestoreRun) createExpiredReplacement(ctx context.Context, node string) error {
	replacement, err := nativeRestoreReaderPod(r.state.Run, node, nativeRestoreReadScript)
	if err != nil {
		return err
	}
	if err := r.advance(ctx, nativeExpiredCreateRequested, nil); err != nil {
		return err
	}
	if err := r.observeDetached(ctx); err != nil {
		return err
	}
	if err := r.verifyExpiredRestorePolicy(ctx); err != nil {
		return err
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	created, err := r.c.create(ctx, r.source, replacement)
	if err != nil {
		return errors.New("the replacement consumer creation could not be confirmed")
	}
	current, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	identity := benchmarkIdentity(created)
	if err != nil || identity.UID == "" || benchmarkIdentity(current) != identity {
		return errNativeRestoreEvidence
	}
	return r.advance(ctx, nativeExpiredReplacement, func(next *nativeRestoreState) {
		evidence := *next.Expired
		evidence.Replacement = identity
		next.Expired = &evidence
	})
}

func (r *nativeRestoreRun) verifyExpiredReplacement(ctx context.Context, node string, velero nativeRestoreVeleroEpoch) error {
	source := r.plan.Seed
	observed := r.state
	observed.Phase, observed.FirstNode = "restore-verified", node
	observed.Resources = maps.Clone(r.state.Resources)
	observed.Resources["FirstPod"] = r.state.Expired.Replacement
	observed.Resources["PodVolumeRestore"] = r.state.Expired.Transfer
	if err := waitReady(ctx, func(ctx context.Context) error {
		if err := r.guard(ctx); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		if _, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, observed, r.config, "", r.plan.Managers); err != nil {
			return err
		}
		if err := r.requireVeleroEpoch(ctx, velero); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		return r.verifyOriginalPayload(ctx, observed, "")
	}); err != nil {
		return err
	}
	return r.advance(ctx, "restore-verified", func(next *nativeRestoreState) {
		next.FirstNode = observed.FirstNode
		next.Resources = observed.Resources
	})
}
