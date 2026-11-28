package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestOpenBaoStageRejectsAChangedPodBeforeReadingDestinations(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	stage := plan.stagePod()
	metadata := valueObject(stage, "metadata")
	metadata["uid"], metadata["resourceVersion"] = "bd879525-004e-400a-be89-719605db0f95", "51"
	changed, err := cloneRecoverySpec(stage)
	if err != nil {
		t.Fatal(err)
	}
	valueObject(changed, "spec", "securityContext")["fsGroup"] = float64(2000)
	calls := 0
	c := client{context: "oidc@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		calls++
		if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" || args[3] != "pod" ||
			args[4] != text(stage, "metadata", "name") {
			t.Fatalf("unexpected request for a changed stage pod: %s %v", program, args)
		}
		return json.Marshal(changed)
	}}
	if _, err := readOpenBaoStagedPair(t.Context(), c, plan, stage, openBaoStagingClaims(plan.Run, plan.Source), nil); err == nil || calls != 1 {
		t.Fatalf("a changed stage pod reached the storage checks, calls=%d err=%v", calls, err)
	}
}

func TestOpenBaoStageReceiptBoundsBothFilesystemResults(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	for _, tc := range []struct {
		log   string
		valid bool
	}{
		{log: "OPENBAO_DESTINATIONS=READY data=102400 audit=409600\n", valid: true},
		{log: "OPENBAO_DESTINATIONS=READY data=0 audit=409600\n"},
		{log: "OPENBAO_DESTINATIONS=READY data=409600"},
		{log: "OPENBAO_DESTINATIONS=READY data=1099511627776 audit=409600"},
		{log: "OPENBAO_DESTINATIONS=READY data=102400 audit=1099511627776"},
		{log: "OPENBAO_DESTINATIONS=READY data=0102400 audit=409600"},
		{log: "OPENBAO_DESTINATIONS=READY data=102400 audit=409600\nsecond result"},
	} {
		if got := verifyOpenBaoStagedFilesystemReceipt(tc.log, plan.Original.Disks); got != tc.valid {
			t.Errorf("receipt %q admitted=%t, want %t", tc.log, got, tc.valid)
		}
	}
}

func TestOpenBaoStagePreservesAnUncertainCreate(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	initial := openBaoPairRecord{Plan: plan, Phase: openBaoPairPlanned}
	raw, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	journal := object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": openBaoPairJournalName(plan.Input.Member), "namespace": openBaoPairJournalNamespace,
			"uid": "f0852bc2-9061-486f-bb48-7c21e12e30a5", "resourceVersion": "100",
			"annotations": object{openBaoCopyFenceOwner: plan.Run + ":" + plan.Source}},
		"data": object{"record": string(raw)},
	}
	creates := 0
	c := client{context: "admin@prod", run: func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(plan.Source + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 3 {
			t.Fatalf("unexpected command %s %v", program, args)
		}
		switch args[2] {
		case "patch":
			var patch []object
			if err := json.Unmarshal(body, &patch); err != nil {
				t.Fatal(err)
			}
			valueObject(journal, "data")["record"] = text(patch[len(patch)-1], "value")
		case "get":
		case "create":
			creates++
			var pending openBaoPairRecord
			var requested object
			if json.Unmarshal([]byte(text(journal, "data", "record")), &pending) != nil || pending.StagePending != "data" ||
				json.Unmarshal(body, &requested) != nil || text(requested, "kind") != "PersistentVolumeClaim" ||
				text(requested, "metadata", "name") != "lh-openbao-staging-data-"+plan.Run {
				t.Fatal("the first destination was requested before the exact creation intent was recorded")
			}
			return nil, errors.New("the API create outcome was not observed")
		default:
			t.Fatalf("unexpected kubectl command %v", args)
		}
		return json.Marshal(journal)
	}}
	run := openBaoPairJournal{client: c, id: benchmarkIdentity(journal), record: initial}
	if err := run.stageDestinations(t.Context(), nil, [2]openBaoStorageRebind{}); err == nil || run.record.StagePending != "data" || creates != 1 {
		t.Fatalf("uncertain destination creation did not hold exactly one intent: %v", err)
	}
	if err := run.stageDestinations(t.Context(), nil, [2]openBaoStorageRebind{}); err == nil || creates != 1 {
		t.Fatal("an unresolved creation was replayed")
	}
}

func TestOpenBaoPairObservationRejectsAnUnreviewedMemberBeforeRequests(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	c := client{context: "oidc@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		t.Fatalf("unreviewed target reached %s %v", program, args)
		return nil, nil
	}}
	if _, err := observeOpenBaoPairPlan(t.Context(), c, "openbao-3", plan.Run, plan.Source); err == nil {
		t.Fatal("the unreviewed member was admitted")
	}
}
