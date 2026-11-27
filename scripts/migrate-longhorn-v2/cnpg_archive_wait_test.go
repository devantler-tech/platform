package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// archiveWaitFixture returns complete source observations and one exact archive response.
func archiveWaitFixture(t *testing.T, mutation string) (client, cnpgRecoveryRequest, cnpgRecoveryPlan, *int) {
	t.Helper()
	c, r, _ := recoveryPlanningFixture(t)
	plan, err := planCNPGRecovery(context.Background(), c, r)
	if err != nil {
		t.Fatal(err)
	}
	delegate := c.run
	probes := 0
	c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" && mutation == "main advanced" && probes > 0 {
			return []byte(strings.Repeat("b", 40) + "\trefs/heads/main\n"), nil
		}
		if program == "kubectl" && len(args) > 3 && args[2] == "exec" && strings.Contains(args[len(args)-1], "archive_status/") {
			probes++
			response := object{"system_id": plan.Barrier.SystemID, "timeline": plan.Barrier.Timeline, "flush_lsn": plan.Barrier.FlushLSN, "in_recovery": false, "target_lsn": plan.Barrier.FlushLSN, "required_wal": "000000070000000200000000", "archive_done": true}
			if mutation == "missing exact segment" {
				response["archive_done"] = false
			}
			if mutation == "another target" {
				response["target_lsn"] = "2/101"
			}
			return json.Marshal(response)
		}
		raw, err := delegate(ctx, input, program, args...)
		if err == nil && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "pod" && probes > 0 && mutation == "process replaced" {
			var pod object
			if json.Unmarshal(raw, &pod) != nil {
				t.Fatal("invalid native Pod fixture")
			}
			pod["metadata"].(object)["uid"] = "replacement"
			return json.Marshal(pod)
		}
		return raw, err
	}
	return c, r, plan, &probes
}

// TestArchiveWaitCollectsTheExactMarkerBetweenStableSourceObservations checks the full read path.
func TestArchiveWaitCollectsTheExactMarkerBetweenStableSourceObservations(t *testing.T) {
	c, r, plan, probes := archiveWaitFixture(t, "")
	if err := waitCNPGRecoveryArchive(context.Background(), c, r, plan); err != nil {
		t.Fatal(err)
	}
	if *probes != 1 {
		t.Fatal("archive success was not backed by the fixed native query")
	}
}

// TestArchiveWaitRefusesTimeoutReplacementAndSourceDrift prevents partial proof escaping.
func TestArchiveWaitRefusesTimeoutReplacementAndSourceDrift(t *testing.T) {
	for _, mutation := range []string{"missing exact segment", "another target", "process replaced", "main advanced"} {
		t.Run(mutation, func(t *testing.T) {
			c, r, plan, _ := archiveWaitFixture(t, mutation)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if waitCNPGRecoveryArchive(ctx, c, r, plan) == nil {
				t.Fatal("incomplete archive observation accepted")
			}
		})
	}
}
