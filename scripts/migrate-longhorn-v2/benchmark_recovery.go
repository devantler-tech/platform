package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Recovery ownership never replaces the source/run that produced the benchmark.
// A new confirmed current-main run may retire scratch assets from a terminal run,
// but must not present those measurements as its own accepted baseline.
type benchmarkRecoveryOwner struct {
	Source, Run, JournalUID string
	Deletes                 map[string]benchmarkResource `json:",omitempty"`
}

var benchmarkRunPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var benchmarkSourcePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validBenchmarkOwner(run, source string) bool {
	if !benchmarkRunPattern.MatchString(run) || !benchmarkSourcePattern.MatchString(source) {
		return false
	}
	_, err := strconv.ParseInt(run, 10, 64)
	return err == nil
}

// Read the latest attempt, rather than attempt 1 alone: a completed first
// attempt does not establish that a later retry has stopped using its objects.
func benchmarkOwnerFinished(ctx context.Context, c client, run, source string) error {
	if !validBenchmarkOwner(run, source) {
		return errors.New("benchmark owner identity is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+run)
	if err != nil || ctx.Err() != nil || len(raw) > 1024*1024 || strictjson.Validate(raw, map[string][]string{"": {"id", "status", "head_sha", "head_branch", "event", "run_attempt", "path"}}) != nil {
		return errors.New("latest benchmark owner attempt is unverified")
	}
	var actual struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		Source  string `json:"head_sha"`
		Branch  string `json:"head_branch"`
		Event   string `json:"event"`
		Attempt uint64 `json:"run_attempt"`
		Path    string `json:"path"`
	}
	wantID, _ := strconv.ParseInt(run, 10, 64)
	if json.Unmarshal(raw, &actual) != nil || actual.ID != wantID || actual.Status != "completed" || actual.Source != source || actual.Branch != "main" || actual.Event != "workflow_dispatch" || actual.Attempt == 0 || !reviewedWorkflowPath(actual.Path) {
		return errors.New("benchmark owner is live, changed or outside the reviewed workflow")
	}
	return nil
}

func claimBenchmarkRecovery(ctx context.Context, c client, phase, source, run string) (benchmarkRecord, error) {
	if (phase != "v1" && phase != "v2") || !validBenchmarkOwner(run, source) {
		return benchmarkRecord{}, errors.New("benchmark recovery identity is invalid")
	}
	cm, record, err := readBenchmark(ctx, c, phase)
	if err != nil || cm == nil || record.Cleaned || run == record.Run {
		return benchmarkRecord{}, errors.New("an incomplete benchmark journal from a different run is required")
	}
	if err := benchmarkOwnerFinished(ctx, c, record.Run, record.Source); err != nil {
		return benchmarkRecord{}, err
	}
	want := &benchmarkRecoveryOwner{Source: source, Run: run, JournalUID: text(cm, "metadata", "uid")}
	if record.Recovery != nil {
		want.Deletes = maps.Clone(record.Recovery.Deletes)
	}
	if record.Recovery != nil && !reflect.DeepEqual(record.Recovery, want) {
		if err := benchmarkOwnerFinished(ctx, c, record.Recovery.Run, record.Recovery.Source); err != nil {
			return benchmarkRecord{}, err
		}
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return benchmarkRecord{}, err
	}
	if reflect.DeepEqual(record.Recovery, want) {
		checkedCM, checked, err := readBenchmark(ctx, c, phase)
		if err != nil || text(checkedCM, "metadata", "uid") != want.JournalUID || !reflect.DeepEqual(record, checked) {
			return benchmarkRecord{}, errors.New("benchmark recovery retry readback changed")
		}
		return checked, nil
	}
	record.Recovery = want
	body, err := json.Marshal(record)
	if err != nil {
		return benchmarkRecord{}, err
	}
	if err := c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(body)}); err != nil {
		return benchmarkRecord{}, errors.New("benchmark recovery claim was not confirmed; preserve the journal")
	}
	checkedCM, checked, err := readBenchmark(ctx, c, phase)
	if err != nil || text(checkedCM, "metadata", "uid") != want.JournalUID || !reflect.DeepEqual(record, checked) {
		return benchmarkRecord{}, errors.New("benchmark recovery claim readback changed")
	}
	return checked, nil
}
