package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

const corootKeeperCutoverOperation = "migrate-coroot-keeper-v2"

var errCorootKeeperReceipt = errors.New("the guarded Keeper migration record changed or is unverified")

type corootKeeperCutoverInput struct {
	Ordinal int
	Digest  string
}

type corootKeeperCutoverRecord struct {
	Version            int
	Run, Source, Phase string
	Input              corootKeeperCutoverInput
	Before             corootKeeperQuorumReceipt
	Coroot             benchmarkResource
	FenceUser          string
	ResumeRun          string `json:",omitempty"`
	ResumeSource       string `json:",omitempty"`
	Creating           string `json:",omitempty"`
	StagedClaim        benchmarkResource
	StagePod           benchmarkResource
	Rebind             corootKeeperRebind
	RollbackStaging    corootKeeperRetainedStage `json:",omitzero"`
	CopyPod            benchmarkResource
	CopyGate           benchmarkResource
	CopyIsolation      benchmarkResource
	PodFence           benchmarkFenceReceipt
	ClaimFence         benchmarkFenceReceipt
	NewClaim           benchmarkResource
}

func authorizeCorootKeeperCutover(getenv func(string) string) (corootKeeperCutoverInput, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	items := strings.Split(confirmation, ":")
	if getenv("LONGHORN_V2_OPERATION") != corootKeeperCutoverOperation || len(items) != 3 ||
		items[0] != corootKeeperCutoverOperation || !retentionDigest.MatchString(items[2]) ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return corootKeeperCutoverInput{}, errCorootKeeperReceipt
	}
	ordinal, err := strconv.Atoi(items[1])
	if err != nil || ordinal < 0 || ordinal > 2 || strconv.Itoa(ordinal) != items[1] {
		return corootKeeperCutoverInput{}, errCorootKeeperReceipt
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return corootKeeperCutoverInput{}, err
	}
	return corootKeeperCutoverInput{Ordinal: ordinal, Digest: items[2]}, nil
}

type corootKeeperJournal struct {
	c        client
	identity benchmarkResource
	record   corootKeeperCutoverRecord
}

func (r corootKeeperCutoverRecord) executor() (run, source string) {
	if r.ResumeRun != "" {
		return r.ResumeRun, r.ResumeSource
	}
	return r.Run, r.Source
}

func (j *corootKeeperJournal) source() string {
	_, source := j.record.executor()
	return source
}

func corootKeeperJournalName(ordinal int) string {
	return "longhorn-v2-coroot-keeper-" + strconv.Itoa(ordinal)
}

func beginCorootKeeperJournal(ctx context.Context, c client, record corootKeeperCutoverRecord) (corootKeeperJournal, error) {
	name := corootKeeperJournalName(record.Input.Ordinal)
	desired, err := json.Marshal(record)
	if err != nil {
		return corootKeeperJournal{}, err
	}
	created, err := c.create(ctx, record.Source, object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": name, "namespace": "longhorn-system",
			"annotations": object{corootKeeperFenceOwner: record.Run + ":" + record.Source}},
		"data": object{"record": string(desired)},
	})
	if err != nil {
		return corootKeeperJournal{}, err
	}
	journal := corootKeeperJournal{c: c, identity: benchmarkIdentity(created), record: record}
	if _, err := journal.read(ctx); err != nil {
		return corootKeeperJournal{}, err
	}
	return journal, nil
}

func (j *corootKeeperJournal) read(ctx context.Context) (object, error) {
	current, err := j.c.get(ctx, "configmap", "longhorn-system", j.identity.Name, false)
	if err != nil {
		return nil, err
	}
	var observed corootKeeperCutoverRecord
	run, source := j.record.executor()
	if !recoveredNativeObject(current, "v1", "ConfigMap", "longhorn-system") ||
		benchmarkIdentity(current) != j.identity || j.record.Version != 1 || !validBenchmarkOwner(j.record.Run, j.record.Source) ||
		!validBenchmarkOwner(run, source) || (j.record.ResumeRun == "") != (j.record.ResumeSource == "") ||
		text(current, "metadata", "annotations", corootKeeperFenceOwner) != j.record.Run+":"+j.record.Source ||
		json.Unmarshal([]byte(text(current, "data", "record")), &observed) != nil ||
		observed != j.record || !validKeeperCreationIntent(observed) {
		return nil, errCorootKeeperReceipt
	}
	return current, nil
}

func (j *corootKeeperJournal) advance(ctx context.Context, next corootKeeperCutoverRecord) error {
	if !validCorootKeeperProgress(j.record, next) {
		return errCorootKeeperReceipt
	}
	return j.write(ctx, next)
}

func (j *corootKeeperJournal) write(ctx context.Context, next corootKeeperCutoverRecord) error {
	current, err := j.read(ctx)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, source := next.executor()
	if err := sourceCurrent(ctx, j.c.run, source); err != nil {
		return err
	}
	if err := j.c.patch(ctx, "configmap", "longhorn-system", current,
		object{"op": "test", "path": "/data/record", "value": text(current, "data", "record")},
		object{"op": "replace", "path": "/data/record", "value": string(encoded)}); err != nil {
		return err
	}
	j.record = next
	_, err = j.read(ctx)
	return err
}

var keeperCutoverPhases = []string{
	"planned", "staging-claim-created", "staging-pod-created", "staging-volume-verified",
	"member-fenced-policy", "member-fenced", "claim-fenced-policy", "claim-fenced", "eviction-requested",
	"copy-isolation-created", "copy-gate-created", "copy-pod-created", "copy-permitted",
	"copy-bytes-verified", "copied-volumes-detached", "claim-release-requested", "original-and-staging-retained",
	"replacement-created", "destination-rebind-requested", "destination-bound", "member-released", "replacement-verified",
}

func validCorootKeeperProgress(previous, next corootKeeperCutoverRecord) bool {
	if previous.Version != 1 || next.Version != previous.Version || next.Run != previous.Run ||
		next.Source != previous.Source || next.Input != previous.Input || next.Before != previous.Before || next.Coroot != previous.Coroot {
		return false
	}
	if next.FenceUser != previous.FenceUser || next.ResumeRun != previous.ResumeRun || next.ResumeSource != previous.ResumeSource {
		return false
	}
	phase := slices.Index(keeperCutoverPhases, previous.Phase)
	if phase < 0 || phase+1 >= len(keeperCutoverPhases) {
		return false
	}
	if next.Creating != "" {
		onlyIntent := previous
		onlyIntent.Creating = next.Creating
		return previous.Creating == "" && next == onlyIntent && validKeeperCreationIntent(next)
	}
	if previous.Creating != "" {
		if !validKeeperCreationIntent(previous) {
			return false
		}
		onlyClear := previous
		onlyClear.Creating = ""
		return next == onlyClear || (next.Phase == previous.Creating && validCorootKeeperProgress(onlyClear, next))
	}
	if next.Phase != keeperCutoverPhases[phase+1] && (next.Phase != "original-claim-preserved" || !mayRestoreOriginalKeeper(previous.Phase)) {
		return false
	}
	if next.Phase != keeperOriginalPreserved && next.RollbackStaging != previous.RollbackStaging {
		return false
	}
	for _, resources := range [][2]benchmarkResource{
		{previous.StagedClaim, next.StagedClaim}, {previous.StagePod, next.StagePod},
		{previous.CopyPod, next.CopyPod}, {previous.CopyGate, next.CopyGate}, {previous.CopyIsolation, next.CopyIsolation},
		{previous.NewClaim, next.NewClaim},
	} {
		if resources[0].UID != "" && resources[0] != resources[1] {
			return false
		}
	}
	if previous.Rebind.StagedVolume.UID != "" && next.Rebind != previous.Rebind {
		return false
	}
	for _, fences := range [][2]benchmarkFenceReceipt{{previous.PodFence, next.PodFence}, {previous.ClaimFence, next.ClaimFence}} {
		if fences[0].PolicyUID != "" && fences[0].PolicyUID != fences[1].PolicyUID ||
			fences[0].BindingUID != "" && fences[0].BindingUID != fences[1].BindingUID {
			return false
		}
	}
	return true
}

func validKeeperCreationIntent(r corootKeeperCutoverRecord) bool {
	if r.Creating == "" {
		return true
	}
	phase := slices.Index(keeperCutoverPhases, r.Phase)
	return phase >= 0 && phase+1 < len(keeperCutoverPhases) &&
		r.Creating == keeperCutoverPhases[phase+1] && isKeeperAssetCreation(r.Creating)
}
