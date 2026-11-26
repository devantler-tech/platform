package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"strings"
	"time"
)

func (r *nativeRestoreRun) restoreFromSeed(ctx context.Context) (nativeRestoreVeleroEpoch, error) {
	velero, err := captureNativeRestoreVelero(ctx, r.c, r.state.Source)
	if err != nil {
		return nativeRestoreVeleroEpoch{}, err
	}
	if err := r.createAsset(ctx, "ConfigMap", "modifier"); err != nil {
		return nativeRestoreVeleroEpoch{}, err
	}
	if err := r.createAsset(ctx, "Restore", "restore"); err != nil {
		return nativeRestoreVeleroEpoch{}, err
	}
	var transfer nativeRestoreTransferReceipt
	err = waitReady(ctx, func(ctx context.Context) error {
		observed, err := r.observeSeedTransfer(ctx, velero)
		if err == nil {
			transfer = observed
		}
		return err
	})
	if err != nil {
		return nativeRestoreVeleroEpoch{}, err
	}
	err = r.advance(ctx, "restore-verified", func(next *nativeRestoreState) {
		next.Resources["FirstPod"], next.Resources["PodVolumeRestore"], next.FirstNode = transfer.Pod, transfer.Transfer, transfer.Node
	})
	return velero, err
}

func (r *nativeRestoreRun) observeSeedTransfer(ctx context.Context, velero nativeRestoreVeleroEpoch) (nativeRestoreTransferReceipt, error) {
	fail := func() (nativeRestoreTransferReceipt, error) {
		return nativeRestoreTransferReceipt{}, errNativeRestoreEvidence
	}
	if err := r.guard(ctx); err != nil {
		return nativeRestoreTransferReceipt{}, errors.Join(errReadinessTerminal, err)
	}
	restoreID := r.state.Resources["Restore"]
	restore, err := r.c.get(ctx, "restores.velero.io", restoreID.Namespace, restoreID.Name, false)
	if err != nil || benchmarkIdentity(restore) != restoreID {
		return fail()
	}
	if err := nativeRestoreTerminalFailure(restore); err != nil {
		return nativeRestoreTransferReceipt{}, err
	}
	pod, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil {
		return fail()
	}
	transfers, err := r.c.list(ctx, "podvolumerestores.velero.io", "velero")
	if err != nil {
		return fail()
	}
	repositories, err := r.c.list(ctx, "backuprepositories.velero.io", "velero")
	if err != nil {
		return fail()
	}
	source := r.plan.Seed
	receipt, err := verifyNativeRestoreTransfer(source.Record, source.Seed, r.state, restore, pod, transfers, repositories)
	if err != nil {
		return fail()
	}
	observed := r.state
	observed.Phase = "restore-verified"
	observed.Resources = maps.Clone(r.state.Resources)
	observed.Resources["FirstPod"], observed.Resources["PodVolumeRestore"], observed.FirstNode = receipt.Pod, receipt.Transfer, receipt.Node
	if _, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, observed, r.config, velero.Digest, r.plan.Managers); err != nil {
		return fail()
	}
	if err := r.requireVeleroEpoch(ctx, velero); err != nil {
		return nativeRestoreTransferReceipt{}, errors.Join(errReadinessTerminal, err)
	}
	output, err := r.execNativeRestorePod(ctx, observed, velero.Digest, nativeRestoreOriginalCheck(source.Seed.PayloadSHA256))
	digest := strings.Fields(string(output))
	if err != nil || len(digest) != 2 || digest[0] != source.Seed.PayloadSHA256 || digest[1] != "payload.bin" {
		return fail()
	}
	return receipt, nil
}

func nativeRestoreTerminalFailure(restore object) error {
	switch text(restore, "status", "phase") {
	case "FailedValidation", "PartiallyFailed", "Failed":
		return errors.Join(errReadinessTerminal, errors.New("the Velero restore ended without a complete result, so preserve the source and restore journal"))
	default:
		return nil
	}
}

func (r *nativeRestoreRun) requireVeleroEpoch(ctx context.Context, expected nativeRestoreVeleroEpoch) error {
	actual, err := captureNativeRestoreVelero(ctx, r.c, r.state.Source)
	if err != nil || actual != expected {
		return errNativeRestoreEvidence
	}
	return nil
}

func (r *nativeRestoreRun) execNativeRestorePod(ctx context.Context, observed nativeRestoreState, helperDigest, script string) ([]byte, error) {
	if err := r.guard(ctx); err != nil {
		return nil, err
	}
	pod, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	source := r.plan.Seed
	if err != nil || verifyNativeRestoreConsumer(source.Record, source.Seed, observed, pod, helperDigest) != nil {
		return nil, errNativeRestoreEvidence
	}
	return r.c.kubectl(ctx, nil, "exec", "--namespace="+benchmarkNamespace, "fio", "--container=fio", "--", "/bin/sh", "-ec", script)
}

func (r *nativeRestoreRun) writeReattachmentToken(ctx context.Context, velero nativeRestoreVeleroEpoch) error {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	digest := r.plan.Seed.Seed.PayloadSHA256
	if err := r.advance(ctx, "write-requested", func(next *nativeRestoreState) { next.Token = token }); err != nil {
		return err
	}
	if err := r.requireVeleroEpoch(ctx, velero); err != nil {
		return err
	}
	source := r.plan.Seed
	before, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, r.state, r.config, velero.Digest, r.plan.Managers)
	if err != nil {
		return err
	}
	script, err := nativeRestoreWriteScript(digest, token, r.state.Resources["FirstPod"].UID)
	if err != nil {
		return err
	}
	result, err := r.execNativeRestorePod(ctx, r.state, velero.Digest, script)
	if err != nil || verifyNativeRestorePayload(string(result), digest, token) != nil {
		return errors.New("the original restore token write could not be confirmed")
	}
	after, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, r.state, r.config, velero.Digest, r.plan.Managers)
	if err != nil || before != after {
		return errNativeRestoreEvidence
	}
	return r.advance(ctx, "write-verified", nil)
}

func (r *nativeRestoreRun) reattach(ctx context.Context, velero nativeRestoreVeleroEpoch) error {
	previous := r.state
	if err := r.advance(ctx, "first-pod-delete-requested", nil); err != nil {
		return err
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	source := r.plan.Seed
	first, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil || verifyNativeRestoreConsumer(source.Record, source.Seed, previous, first, velero.Digest) != nil {
		return errNativeRestoreEvidence
	}
	if err := r.c.deleteObserved(ctx, r.state.Source, "/api/v1/namespaces/"+benchmarkNamespace+"/pods/fio", first); err != nil {
		return errors.New("the original restore consumer deletion could not be confirmed")
	}
	if err := waitReady(ctx, r.observeDetached); err != nil {
		return err
	}
	if err := r.advance(ctx, "first-pod-deleted", nil); err != nil {
		return err
	}
	targets := make(map[string]string, len(r.state.Targets))
	for _, target := range r.state.Targets {
		targets[target.Name] = target.UID
	}
	node := r.state.Targets[1].Name
	asset, err := nativeRestoreReattachPod(source.Record, source.Seed, nativeReattachRequest{
		Run: r.state.Run, Token: r.state.Token, FirstNode: r.state.FirstNode, Target: node, Targets: targets,
	})
	if err != nil {
		return err
	}
	if err := r.advance(ctx, "reattach-create-requested", nil); err != nil {
		return err
	}
	if err := r.observeDetached(ctx); err != nil {
		return err
	}
	created, err := r.c.create(ctx, r.state.Source, asset)
	if err != nil {
		return errors.New("the reattachment consumer creation could not be confirmed")
	}
	id := benchmarkIdentity(created)
	current, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", false)
	if err != nil || id.UID == "" || id == r.state.Resources["FirstPod"] || benchmarkIdentity(current) != id {
		return errNativeRestoreEvidence
	}
	if err = r.advance(ctx, "reattach-owned", func(next *nativeRestoreState) { next.Resources["SecondPod"] = id }); err != nil {
		return err
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		if err := r.guard(ctx); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		_, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, r.state, r.config, "", r.plan.Managers)
		return err
	}); err != nil {
		return err
	}
	script, err := nativeRestoreVerifyScript(source.Seed.PayloadSHA256, r.state.Token)
	if err != nil {
		return err
	}
	output, err := r.execNativeRestorePod(ctx, r.state, "", script)
	if err != nil || verifyNativeRestorePayload(string(output), source.Seed.PayloadSHA256, r.state.Token) != nil {
		return errNativeRestoreEvidence
	}
	if _, err := observeNativeRestoreAttached(ctx, r.c, source.Record, source.Seed, r.state, r.config, "", r.plan.Managers); err != nil {
		return err
	}
	if err := r.advance(ctx, "reattach-verified", func(next *nativeRestoreState) { next.SecondNode = node }); err != nil {
		return err
	}
	return r.advance(ctx, "completed", func(next *nativeRestoreState) {
		next.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	})
}

func (r *nativeRestoreRun) observeDetached(ctx context.Context) error {
	if err := r.guard(ctx); err != nil {
		return errors.Join(errReadinessTerminal, err)
	}
	return observeNativeRestoreDetached(ctx, r.c, r.plan.Seed.Record, r.plan.Seed.Seed, r.state)
}

func runNativeRestore(ctx context.Context, c client, source, run string, config []byte) error {
	plan, err := observeNativeRestoreAdmission(ctx, c, source, run, config)
	if err != nil {
		return err
	}
	restore := nativeRestoreRun{c: c, config: config, plan: plan}
	if err := restore.provision(ctx); err != nil {
		return err
	}
	velero, err := restore.restoreFromSeed(ctx)
	if err != nil {
		return err
	}
	if err := restore.writeReattachmentToken(ctx, velero); err != nil {
		return err
	}
	return restore.reattach(ctx, velero)
}
