package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
)

func seedSpecDigest(o object) (string, error) {
	spec, ok := value(o, "spec").(object)
	if !ok || len(spec) == 0 {
		return "", errSeedEvidence
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", errSeedEvidence
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func readCanarySeedJournal(ctx context.Context, c client) (object, benchmarkRecord, canarySeedState, error) {
	if !c.nativeSeed {
		return nil, benchmarkRecord{}, canarySeedState{}, errSeedEvidence
	}
	cm, record, err := readBenchmark(ctx, c, "v1")
	if err != nil || cm == nil || text(cm, "metadata", "name") != nativeSeedJournal || text(cm, "metadata", "namespace") != "longhorn-system" ||
		text(cm, "metadata", "uid") == "" || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil {
		return nil, benchmarkRecord{}, canarySeedState{}, errSeedEvidence
	}
	state, err := decodeCanarySeedState(text(cm, "data", "seed"), record)
	if err != nil {
		return nil, benchmarkRecord{}, canarySeedState{}, errSeedEvidence
	}
	return cm, record, state, nil
}

// Each phase advances once through the original ConfigMap UID and revision.
// An uncertain PATCH or failed readback stops; neither is retried or adopted.
func saveCanarySeedState(ctx context.Context, c client, journalUID string, before, after canarySeedState) error {
	cm, record, actual, err := readCanarySeedJournal(ctx, c)
	if err != nil || journalUID == "" || text(cm, "metadata", "uid") != journalUID || actual != before ||
		!activeBenchmarkV1FenceOwner(c, record) || record.Source != os.Getenv("GITHUB_SHA") || record.Run != os.Getenv("GITHUB_RUN_ID") ||
		validateSeedTransition(before, after, record) != nil {
		return errSeedEvidence
	}
	body, err := json.Marshal(after)
	if err != nil {
		return errSeedEvidence
	}
	if err = sourceCurrent(ctx, c.run, record.Source); err != nil {
		return errors.New("the reviewed source changed before canary progress")
	}
	if err = c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/seed", "value": string(body)}); err != nil {
		return errors.New("canary seed progress could not be confirmed; preserve the journal")
	}
	checked, current, saved, err := readCanarySeedJournal(ctx, c)
	if err != nil || text(checked, "metadata", "uid") != journalUID || saved != after || !reflect.DeepEqual(current, record) {
		return errors.New("canary seed progress readback changed; preserve the journal")
	}
	return nil
}
