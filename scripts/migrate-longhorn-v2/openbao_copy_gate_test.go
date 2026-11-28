package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestOpenBaoCopyGateIsBoundToItsRecordedIdentity(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	gate := openBaoCopyGate(plan.Run, plan.Source)
	metadata := valueObject(gate, "metadata")
	metadata["uid"], metadata["resourceVersion"] = "9c704bd6-b18b-4190-9ccc-db6c43a96207", "80"
	if !matchOpenBaoCopyGate(gate, gate, plan, "hold") {
		t.Fatal("the exact held gate was refused")
	}
	replaced, err := cloneRecoverySpec(gate)
	if err != nil {
		t.Fatal(err)
	}
	valueObject(replaced, "metadata")["uid"] = "c49d8d0e-28f2-4299-a2a1-cb0a8ef66f02"
	if matchOpenBaoCopyGate(replaced, gate, plan, "hold") {
		t.Fatal("the same-named replacement gate was accepted")
	}
	valueObject(gate, "data")["state"] = "release"
	if matchOpenBaoCopyGate(gate, gate, plan, plan.Run+":"+plan.Source) {
		t.Fatal("an unexpected projected approval was accepted")
	}
}

func TestOpenBaoCopyReleaseAndReceiptRequireTheProtectedExecutor(t *testing.T) {
	j := openBaoPairJournal{
		record: openBaoPairRecord{Plan: openBaoPairPlanFixture(t), Phase: openBaoPairStaged},
		client: client{context: "oidc@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			t.Fatalf("the read-only context attempted %s %v", program, args)
			return nil, nil
		}},
	}
	if _, err := j.waitAndReleaseCopyGate(t.Context(), nil, nil, nil, "", nil); err == nil {
		t.Fatal("read-only access approved a copy")
	}
	if err := j.recordVerifiedCopy(t.Context(), nil, nil, recoveryCiliumDatapath{PolicyUID: "synthetic"}, "", nil); err == nil {
		t.Fatal("read-only access advanced the copy journal")
	}
}

func testOpenBaoCopyGateWaitsForScheduling(t *testing.T, record openBaoPairRecord) {
	t.Helper()
	originalPoll := readinessPoll
	readinessPoll = time.Millisecond
	t.Cleanup(func() { readinessPoll = originalPoll })

	assets := desiredOpenBaoCopyAssets(record.Plan)
	for index, asset := range assets {
		metadata := valueObject(asset, "metadata")
		metadata["uid"], metadata["resourceVersion"] = record.CopyAssets[index].UID, "76"
	}
	pod := assets[2]
	previous, err := cloneRecoverySpec(pod)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || len(args) < 5 || args[2] != "get" || args[3] != "pod" ||
			args[4] != record.CopyAssets[2].Name {
			t.Fatalf("the unqualified copy reached %s %v", program, args)
		}
		requests++
		if requests > 1 {
			valueObject(pod, "spec")["nodeName"] = "unexpected-worker"
		}
		return json.Marshal(pod)
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	j := openBaoPairJournal{client: c, record: record}
	_, err = j.waitAndReleaseCopyGate(ctx, previous, assets[0], assets[1], "", nil)
	if requests != 2 || !errors.Is(err, errReadinessTerminal) {
		t.Fatalf("want a retry for the unscheduled pod and a terminal refusal on another node, got reads=%d, err=%v", requests, err)
	}
}
