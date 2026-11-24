package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
)

const (
	nativeRestoreOperation    = "restore-native-v2-seed"
	nativeRestoreConfirmation = "restore-longhorn-native-v2-seed"
	nativeRestoreJournal      = "longhorn-v2-native-restore"
)

// Require the distinct restore operation on a first attempt from reviewed main.
func authorizeNativeRestore(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != nativeRestoreOperation || getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errNativeRestoreEvidence
	}
	return authorizeWorkflow(getenv, nativeRestoreConfirmation)
}

// The runner calls this after preparing and qualifying the target roster and
// confirming that all restore asset names are free. The journal is the first
// write. An existing journal or uncertain response stops the run.
func createNativeRestoreJournal(ctx context.Context, c client, seed nativeRestoreSeedReceipt, state nativeRestoreState) (string, error) {
	if !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed || authorizeNativeRestore(os.Getenv) != nil ||
		state.Source != os.Getenv("GITHUB_SHA") || state.Run != os.Getenv("GITHUB_RUN_ID") ||
		state.Phase != "created" || state.SeedJournal != seed.Journal || state.validate(seed.Record, seed.Seed) != nil {
		return "", errNativeRestoreEvidence
	}
	old, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreJournal, true)
	if err != nil || old != nil {
		return "", errors.New("a prior restore journal is present or unreadable; preserve it for recovery")
	}
	seedClient := c
	seedClient.nativeSeed = true
	current, record, checkedSeed, err := readCanarySeedJournal(ctx, seedClient)
	if err != nil || benchmarkIdentity(current) != seed.Journal || !reflect.DeepEqual(record, seed.Record) || checkedSeed != seed.Seed {
		return "", errNativeRestoreEvidence
	}
	body, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	created, err := c.create(ctx, state.Source, object{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeRestoreJournal, "namespace": "longhorn-system"},
		"data": object{"restore": string(body)},
	})
	id := benchmarkIdentity(created)
	if err != nil || !nativeRestoreUID(id.UID) || id.Namespace != "longhorn-system" ||
		nativeRestoreReceiptUIDs(record, checkedSeed, state)[id.UID] {
		return "", errors.New("the native restore journal CREATE could not be confirmed; preserve it without adoption or retry")
	}
	actual, afterRecord, afterSeed, saved, err := readNativeRestoreJournal(ctx, c)
	if err != nil || benchmarkIdentity(actual) != id || !reflect.DeepEqual(saved, state) ||
		!reflect.DeepEqual(afterRecord, record) || afterSeed != checkedSeed {
		return "", errors.New("the native restore journal readback changed; preserve both journals")
	}
	return id.UID, nil
}

func readNativeRestoreJournal(ctx context.Context, c client) (object, benchmarkRecord, canarySeedState, nativeRestoreState, error) {
	cm, err := c.get(ctx, "configmap", "longhorn-system", nativeRestoreJournal, false)
	data, ok := value(cm, "data").(object)
	if err != nil || !ok || len(data) != 1 || text(cm, "apiVersion") != "v1" || text(cm, "kind") != "ConfigMap" ||
		text(cm, "metadata", "name") != nativeRestoreJournal || text(cm, "metadata", "namespace") != "longhorn-system" ||
		!nativeRestoreUID(text(cm, "metadata", "uid")) || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil {
		return nil, benchmarkRecord{}, canarySeedState{}, nativeRestoreState{}, errNativeRestoreEvidence
	}
	sourceClient := c
	sourceClient.nativeSeed = true
	source, record, seed, err := readCanarySeedJournal(ctx, sourceClient)
	if err != nil {
		return nil, benchmarkRecord{}, canarySeedState{}, nativeRestoreState{}, errNativeRestoreEvidence
	}
	state, err := decodeNativeRestoreState(text(cm, "data", "restore"), record, seed)
	if err != nil || state.SeedJournal != benchmarkIdentity(source) {
		return nil, benchmarkRecord{}, canarySeedState{}, nativeRestoreState{}, errNativeRestoreEvidence
	}
	return cm, record, seed, state, nil
}

// Only the active first-attempt reviewed-main owner can advance the original
// restore journal. Never PATCH the seed journal, retry a write, or adopt a
// replaced object or a response whose outcome cannot be confirmed.
func saveNativeRestoreState(ctx context.Context, c client, journalUID string, before, after nativeRestoreState) error {
	if c.context != "admin@prod" || authorizeNativeRestore(os.Getenv) != nil || !nativeRestoreUID(journalUID) ||
		before.Source != os.Getenv("GITHUB_SHA") || before.Run != os.Getenv("GITHUB_RUN_ID") {
		return errNativeRestoreEvidence
	}
	cm, record, seed, actual, err := readNativeRestoreJournal(ctx, c)
	if err != nil || text(cm, "metadata", "uid") != journalUID || !reflect.DeepEqual(actual, before) || validateNativeRestoreTransition(before, after, record, seed) != nil {
		return errNativeRestoreEvidence
	}
	body, err := json.Marshal(after)
	if err != nil {
		return errNativeRestoreEvidence
	}
	if err = sourceCurrent(ctx, c.run, before.Source); err != nil {
		return errors.New("the reviewed source changed before native restore progress")
	}
	if err = c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/restore", "value": string(body)}); err != nil {
		return errors.New("native restore progress could not be confirmed; preserve both original journals")
	}
	checked, currentRecord, currentSeed, saved, err := readNativeRestoreJournal(ctx, c)
	if err != nil || text(checked, "metadata", "uid") != journalUID || !reflect.DeepEqual(saved, after) || !reflect.DeepEqual(currentRecord, record) || currentSeed != seed {
		return errors.New("native restore progress readback changed; preserve both original journals")
	}
	return nil
}
