package main

import (
	"context"
	"encoding/json"
	"testing"
)

// TestArchiveReadRejectsAnUnparsedCoordinateBeforeAnyCommand protects the fixed SQL template.
func TestArchiveReadRejectsAnUnparsedCoordinateBeforeAnyCommand(t *testing.T) {
	for _, target := range []string{"2/100;SELECT 1", "2/100'", "0/0", "2/100\n", "../../other", "2/100000000"} {
		c := client{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("invalid recovery coordinate reached a command")
			return nil, nil
		}}
		if _, err := c.readPostgresSQL(context.Background(), nil, nil, target); err == nil {
			t.Fatalf("invalid coordinate accepted: %q", target)
		}
	}
}

// archiveMarkerFixture describes one fixed-query response from the bound primary.
func archiveMarkerFixture(t *testing.T) (object, postgresState) {
	t.Helper()
	barrier := postgresState{SystemID: "7470000000000000001", Timeline: "7", FlushLSN: "2/100"}
	return object{"system_id": barrier.SystemID, "timeline": barrier.Timeline, "flush_lsn": "2/200", "in_recovery": false, "target_lsn": barrier.FlushLSN, "required_wal": "000000070000000200000000", "archive_done": true}, barrier
}

// TestRecoveryArchiveRequiresItsExactCompletedSegment distinguishes pending from proven archival.
func TestRecoveryArchiveRequiresItsExactCompletedSegment(t *testing.T) {
	response, barrier := archiveMarkerFixture(t)
	raw, _ := json.Marshal(response)
	if done, err := verifyCNPGArchiveMarker(raw, barrier); err != nil || !done {
		t.Fatalf("exact completed segment refused: %v", err)
	}
	response["archive_done"] = false
	raw, _ = json.Marshal(response)
	if done, err := verifyCNPGArchiveMarker(raw, barrier); err != nil || done {
		t.Fatalf("pending exact segment became proof: %v", err)
	}
}

// TestRecoveryArchiveRefusesIncompleteOrChangedPhysicalBindings keeps archival on one history.
func TestRecoveryArchiveRefusesIncompleteOrChangedPhysicalBindings(t *testing.T) {
	for name, change := range map[string]func(object){
		"missing marker":   func(o object) { delete(o, "archive_done") },
		"null marker":      func(o object) { o["archive_done"] = nil },
		"string marker":    func(o object) { o["archive_done"] = "true" },
		"missing role":     func(o object) { delete(o, "in_recovery") },
		"standby":          func(o object) { o["in_recovery"] = true; o["replay_lsn"] = "2/200" },
		"physical source":  func(o object) { o["system_id"] = "7470000000000000002" },
		"timeline":         func(o object) { o["timeline"] = "8" },
		"target":           func(o object) { o["target_lsn"] = "2/101" },
		"regressed source": func(o object) { o["flush_lsn"] = "2/80" },
		"segment history":  func(o object) { o["required_wal"] = "000000080000000200000000" },
		"not a segment":    func(o object) { o["required_wal"] = "000000070000000200000000.backup" },
		"later statistic":  func(o object) { delete(o, "archive_done"); o["last_archived_wal"] = "000000070000000200000001" },
	} {
		t.Run(name, func(t *testing.T) {
			response, barrier := archiveMarkerFixture(t)
			change(response)
			raw, _ := json.Marshal(response)
			if done, err := verifyCNPGArchiveMarker(raw, barrier); err == nil || done {
				t.Fatal("incomplete or changed archive witness accepted")
			}
		})
	}
}
