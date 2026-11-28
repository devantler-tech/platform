package main

import (
	"context"
	"encoding/json"
)

func bindOpenBaoStageReuse(previous openBaoPairRecord, next openBaoPairPlan, confirmedRun string) (openBaoPairPlan, [3]benchmarkResource, error) {
	if !previous.valid() || !previous.RolledBack || previous.Phase != openBaoPairStaged || previous.Evicting ||
		previous.Plan.Run != confirmedRun || previous.Plan.Run == next.Run || previous.Plan.Original != next.Original ||
		next.StagingRun != "" || next.StagingSource != "" {
		return openBaoPairPlan{}, [3]benchmarkResource{}, errOpenBaoProviderStorage
	}
	next.StagingRun, next.StagingSource = previous.Plan.stagingOwner()
	if err := verifyOpenBaoPairPlan(next, next.Run, next.Source); err != nil {
		return openBaoPairPlan{}, [3]benchmarkResource{}, err
	}
	return next, [3]benchmarkResource{previous.StageAssets[0], previous.StageAssets[1]}, nil
}

func observeReusableOpenBaoStage(ctx context.Context, c client, next openBaoPairPlan, confirmedRun, token string) (openBaoPairRecord, string, error) {
	name := openBaoPairJournalName(next.Input.Member)
	current, err := c.get(ctx, "configmap", openBaoPairJournalNamespace, name, true)
	if err != nil {
		return openBaoPairRecord{}, "", err
	}
	if current == nil {
		// A previous retry may have archived the old journal before creating
		// its successor. The explicit run only permits that exact archive.
		name += "-attempt-" + confirmedRun
		current, err = c.get(ctx, "configmap", openBaoPairJournalNamespace, name, false)
	}
	payload := text(current, "data", "record")
	var previous openBaoPairRecord
	if err != nil || json.Unmarshal([]byte(payload), &previous) != nil ||
		!recoveredNativeObject(current, "v1", "ConfigMap", openBaoPairJournalNamespace) ||
		text(current, "metadata", "name") != name ||
		text(current, "metadata", "annotations", openBaoCopyFenceOwner) != previous.Plan.Run+":"+previous.Plan.Source {
		return openBaoPairRecord{}, "", errOpenBaoProviderStorage
	}
	if _, _, err := bindOpenBaoStageReuse(previous, next, confirmedRun); err != nil {
		return openBaoPairRecord{}, "", err
	}
	if err := verifyInterruptedOpenBaoAttempt(ctx, c, previous.Plan.Run, previous.Plan.Source); err != nil {
		return openBaoPairRecord{}, "", err
	}
	j := openBaoPairJournal{client: c, record: previous}
	storage, err := j.readCopyStorage(ctx, token, 3, true)
	if err != nil {
		return openBaoPairRecord{}, "", err
	}
	desired := openBaoStagingClaims(previous.Plan.stagingOwner())
	for i, staged := range previous.Destinations {
		claim, err := storage.claimForRelease(staged.StagingClaim, staged.DestinationPV, "longhorn-v2", "driver.longhorn.io",
			staged.DestinationVolume.Name, staged.Source.Bytes)
		if err != nil || !matchOpenBaoStagedClaim(claim, desired[i]) {
			return openBaoPairRecord{}, "", errOpenBaoProviderStorage
		}
	}
	if err := verifyOpenBaoDestinationsDetached(ctx, c, previous.Destinations); err != nil {
		return openBaoPairRecord{}, "", err
	}
	return previous, payload, nil
}

func archiveRestoredOpenBaoAttempt(ctx context.Context, c client, next openBaoPairPlan, token, expected string) error {
	name := openBaoPairJournalName(next.Input.Member)
	active, err := c.get(ctx, "configmap", openBaoPairJournalNamespace, name, true)
	if err != nil || active == nil {
		return err
	}
	payload := text(active, "data", "record")
	var previous openBaoPairRecord
	if (expected != "" && payload != expected) || json.Unmarshal([]byte(payload), &previous) != nil || !previous.valid() || !previous.RolledBack ||
		previous.Plan.Input.Member != next.Input.Member || previous.Plan.Run == next.Run {
		return errOpenBaoProviderStorage
	}
	j := openBaoPairJournal{client: c, id: benchmarkIdentity(active), record: previous}
	if _, err := j.read(ctx); err != nil {
		return err
	}
	if err := verifyInterruptedOpenBaoAttempt(ctx, c, previous.Plan.Run, previous.Plan.Source); err != nil {
		return err
	}
	if _, err := j.readServingOriginalPair(ctx, token); err != nil {
		return err
	}
	for _, id := range []benchmarkResource{previous.StageAssets[2], previous.CopyAssets[2]} {
		if id.UID == "" {
			continue
		}
		pod, err := c.get(ctx, "pod", openBaoNamespace, id.Name, true)
		if err != nil || pod != nil {
			return errOpenBaoProviderStorage
		}
	}
	for _, index := range []int{0, 2} {
		fence := previous.FenceAssets[index]
		if fence.UID == "" {
			continue
		}
		for _, kind := range []string{keeperPolicyKind, keeperBindingKind} {
			policy, err := c.get(ctx, kind, "", fence.Name, true)
			if err != nil || policy != nil {
				return errOpenBaoProviderStorage
			}
		}
	}
	if err := writeRestoredOpenBaoArchive(ctx, c, next.Source, previous, payload); err != nil {
		return err
	}
	current, err := j.read(ctx)
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + openBaoPairJournalNamespace + "/configmaps/" + name
	if err := c.deleteObserved(ctx, next.Source, path, current); err != nil {
		return err
	}
	return waitReady(ctx, func(ctx context.Context) error {
		remaining, err := c.get(ctx, "configmap", openBaoPairJournalNamespace, name, true)
		if err != nil || remaining != nil {
			return errOpenBaoProviderStorage
		}
		return nil
	})
}

func writeRestoredOpenBaoArchive(ctx context.Context, c client, source string, previous openBaoPairRecord, payload string) error {
	plan := previous.Plan
	name := openBaoPairJournalName(plan.Input.Member) + "-attempt-" + plan.Run
	owner := plan.Run + ":" + plan.Source
	archive, err := c.get(ctx, "configmap", openBaoPairJournalNamespace, name, true)
	if err == nil && archive == nil {
		archive, err = c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": object{"name": name, "namespace": openBaoPairJournalNamespace,
				"annotations": object{openBaoCopyFenceOwner: owner}},
			"data": object{"record": payload}})
	}
	if err != nil || !recoveredNativeObject(archive, "v1", "ConfigMap", openBaoPairJournalNamespace) ||
		text(archive, "metadata", "name") != name || text(archive, "metadata", "annotations", openBaoCopyFenceOwner) != owner ||
		text(archive, "data", "record") != payload {
		return errOpenBaoProviderStorage
	}
	return nil
}
