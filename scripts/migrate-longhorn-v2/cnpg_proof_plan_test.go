package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestDatabaseRecoveryPlanWitnessSurvivesJSONButCannotChange protects its persisted target.
func TestDatabaseRecoveryPlanWitnessSurvivesJSONButCannotChange(t *testing.T) {
	c, request, _ := recoveryPlanningFixture(t)
	plan, err := planCNPGRecovery(context.Background(), c, request)
	if err != nil {
		t.Fatal(err)
	}
	before := cnpgProofRecord{Version: 1, Source: request.Source, Run: request.Run, Target: request.Target, Phase: "restore-planned", Archive: request.Archive, Backup: request.Backup, Plan: &plan}
	raw, err := encodeCNPGProofRecord(before)
	var after cnpgProofRecord
	if err != nil || json.Unmarshal([]byte(raw), &after) != nil || !unchangedCNPGProofInputs(before, after) {
		t.Fatal("native JSON round-trip changed the recovery witness")
	}
	after.Plan.Barrier.FlushLSN = "2/200"
	if unchangedCNPGProofInputs(before, after) {
		t.Fatal("persisted WAL target could be replaced")
	}
}

// plannedProofFixture connects the recorded native Backup, archive and journal clients.
func plannedProofFixture(t *testing.T, mutation string) (*cnpgProofRun, *int) {
	t.Helper()
	c, request, _, _ := archiveWaitFixture(t, mutation)
	record := cnpgProofRecord{Version: 1, Source: request.Source, Run: request.Run, Target: request.Target, Phase: "backup-created", Archive: request.Archive, Backup: request.Backup, Resources: map[string]benchmarkResource{}}
	cm := ownedCNPGJournalFixture(t, record)
	delegate := c.run
	writes := 0
	c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "configmap" {
			return json.Marshal(cm)
		}
		if program == "kubectl" && len(args) > 3 && args[2] == "patch" {
			writes++
			var operations []object
			if json.Unmarshal(input, &operations) != nil || len(operations) != 3 || text(operations[0], "path") != "/metadata/uid" || operations[0]["value"] != text(cm, "metadata", "uid") || text(operations[1], "path") != "/metadata/resourceVersion" || operations[1]["value"] != text(cm, "metadata", "resourceVersion") {
				t.Fatal("planned restore lost its journal CAS")
			}
			cm["data"].(object)["record"] = text(operations[2], "value")
			cm["metadata"].(object)["resourceVersion"] = "20"
			return []byte("configmap/proof\n"), nil
		}
		return delegate(ctx, input, program, args...)
	}
	return &cnpgProofRun{c: c, record: record, journalUID: text(cm, "metadata", "uid")}, &writes
}

// TestDatabaseRecoveryPlanningCommitsOnlyAfterNativeBackupAndArchiveProof checks the assembled path.
func TestDatabaseRecoveryPlanningCommitsOnlyAfterNativeBackupAndArchiveProof(t *testing.T) {
	p, writes := plannedProofFixture(t, "")
	if err := p.prepareRecovery(context.Background(), "qualified-worker"); err != nil {
		t.Fatal(err)
	}
	if *writes != 1 || p.record.Phase != "restore-planned" {
		t.Fatal("completed native recovery plan was not durably recorded")
	}
}

// TestDatabaseRecoveryPlanningCannotCheckpointIncompleteNativeProof prevents partial readiness.
func TestDatabaseRecoveryPlanningCannotCheckpointIncompleteNativeProof(t *testing.T) {
	for _, mutation := range []string{"missing exact segment", "another target", "process replaced", "main advanced"} {
		t.Run(mutation, func(t *testing.T) {
			p, writes := plannedProofFixture(t, mutation)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if p.prepareRecovery(ctx, "qualified-worker") == nil || *writes != 0 || p.record.Phase != "backup-created" {
				t.Fatal("incomplete native proof was checkpointed")
			}
		})
	}
}
