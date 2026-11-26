package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
)

const nativeRestoreReclaimJournal = "longhorn-v2-native-reclaim"

type nativeRestoreReclaimProgress struct {
	Source, Run, OriginalRun, RestoreJournalUID, Phase string
	RestoreExecutor                                    string `json:",omitempty"`
	Fences                                             map[string]benchmarkFenceReceipt
}

type nativeRestoreReclaim struct {
	c        client
	inputs   nativeRestoreReclaimInputs
	journal  benchmarkResource
	progress nativeRestoreReclaimProgress
}

func beginNativeRestoreReclaim(ctx context.Context, c client, inputs nativeRestoreReclaimInputs) (nativeRestoreReclaim, error) {
	if owner, err := authorizedNativeRestoreReclaimRun(os.Getenv); err != nil || owner != inputs.state.Run || inputs.rebind(ctx, c) != nil {
		return nativeRestoreReclaim{}, errNativeRestoreEvidence
	}
	old, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreReclaimJournal, true)
	if err != nil || old != nil {
		return nativeRestoreReclaim{}, errors.New("a prior reclaim journal is present or unreadable")
	}
	r := nativeRestoreReclaim{c: c, inputs: inputs, progress: nativeRestoreReclaimProgress{
		Source: os.Getenv("GITHUB_SHA"), Run: os.Getenv("GITHUB_RUN_ID"), OriginalRun: inputs.state.Run,
		RestoreJournalUID: inputs.journal.UID, RestoreExecutor: inputs.executor, Phase: "created", Fences: map[string]benchmarkFenceReceipt{},
	}}
	raw, err := json.Marshal(r.progress)
	if err != nil {
		return nativeRestoreReclaim{}, err
	}
	created, err := c.create(ctx, r.progress.Source, object{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeRestoreReclaimJournal, "namespace": "longhorn-system"},
		"data": object{"reclaim": string(raw)},
	})
	if err != nil {
		return nativeRestoreReclaim{}, errors.New("reclaim journal creation was not confirmed")
	}
	r.journal = benchmarkIdentity(created)
	if !nativeRestoreUID(r.journal.UID) || r.journal.UID == inputs.journal.UID ||
		nativeRestoreReceiptUIDs(inputs.record, inputs.seed, inputs.state)[r.journal.UID] {
		return nativeRestoreReclaim{}, errNativeRestoreEvidence
	}
	if _, err := r.guard(ctx); err != nil {
		return nativeRestoreReclaim{}, err
	}
	return r, nil
}

func (r nativeRestoreReclaim) guard(ctx context.Context) (object, error) {
	if owner, err := authorizedNativeRestoreReclaimRun(os.Getenv); err != nil || owner != r.inputs.state.Run ||
		r.progress.Source != os.Getenv("GITHUB_SHA") || r.progress.Run != os.Getenv("GITHUB_RUN_ID") ||
		r.inputs.rebind(ctx, r.c) != nil {
		return nil, errNativeRestoreEvidence
	}
	checked, err := r.c.get(ctx, "configmap", "longhorn-system", nativeRestoreReclaimJournal, false)
	expected, marshalErr := json.Marshal(r.progress)
	if err != nil || marshalErr != nil || benchmarkIdentity(checked) != r.journal || value(checked, "metadata", "deletionTimestamp") != nil ||
		text(checked, "apiVersion") != "v1" || text(checked, "kind") != "ConfigMap" || len(valueObject(checked, "data")) != 1 ||
		text(checked, "data", "reclaim") != string(expected) || sourceCurrent(ctx, r.c.run, r.progress.Source) != nil {
		return nil, errNativeRestoreEvidence
	}
	return checked, nil
}

func (r *nativeRestoreReclaim) advance(ctx context.Context, phase string, fenceName string, fence benchmarkFenceReceipt) error {
	current, err := r.guard(ctx)
	if err != nil {
		return err
	}
	next := r.progress
	next.Phase = phase
	if fenceName != "" {
		next.Fences = maps.Clone(r.progress.Fences)
		next.Fences[fenceName] = fence
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := r.c.patch(ctx, "configmap", "longhorn-system", current, object{"op": "replace", "path": "/data/reclaim", "value": string(raw)}); err != nil {
		return errors.New("reclaim progress could not be confirmed")
	}
	r.progress = next
	_, err = r.guard(ctx)
	return err
}
