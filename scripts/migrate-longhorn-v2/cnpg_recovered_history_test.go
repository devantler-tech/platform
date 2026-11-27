package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// recoveredHistoryFixture models a promoted clone after a normal restart.
func recoveredHistoryFixture() (object, postgresState) {
	barrier := postgresState{SystemID: "7470000000000000001", Timeline: "7", FlushLSN: "2/100"}
	response := object{"system_id": barrier.SystemID, "timeline": "8", "flush_lsn": "2/200", "replay_lsn": nil, "in_recovery": false, "postmaster_start": "2026-11-12 07:00:00.000000+00", "data_directory": "/var/lib/postgresql/data/pgdata", "tablespaces_builtin_only": true, "tblspc_empty": true, "timeline_history": "1\t0/100\tprior promotion\n7\t2/100\tafter LSN 2/100\n"}
	return response, barrier
}

func TestRecoveredHistoryRequiresTheActualServerDataDirectory(t *testing.T) {
	for name, mutate := range map[string]func(object){
		"missing":              func(o object) { delete(o, "data_directory") },
		"null":                 func(o object) { o["data_directory"] = nil },
		"empty":                func(o object) { o["data_directory"] = "" },
		"container filesystem": func(o object) { o["data_directory"] = "/tmp/pgdata" },
		"mount root":           func(o object) { o["data_directory"] = "/var/lib/postgresql/data" },
		"unclean":              func(o object) { o["data_directory"] = "/var/lib/postgresql/data/other/../pgdata" },
		"missing postmaster":   func(o object) { delete(o, "postmaster_start") },
		"null postmaster":      func(o object) { o["postmaster_start"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			response, barrier := recoveredHistoryFixture()
			mutate(response)
			raw, _ := json.Marshal(response)
			if verifyCNPGRecoveredHistory(raw, barrier) == nil {
				t.Fatal("physical history from an unproven server directory or process accepted")
			}
		})
	}
}

func TestRecoveredHistoryRefusesAmbiguousNativeEvidence(t *testing.T) {
	response, barrier := recoveredHistoryFixture()
	raw, _ := json.Marshal(response)
	for name, addition := range map[string]string{
		"duplicate directory":         `,"data_directory":"/var/lib/postgresql/data/pgdata"`,
		"case alias":                  `,"DATA_DIRECTORY":"/var/lib/postgresql/data/pgdata"`,
		"unknown field":               `,"unexpected":true`,
		"duplicate physical identity": `,"system_id":"7470000000000000001"`,
	} {
		t.Run(name, func(t *testing.T) {
			ambiguous := append(append([]byte{}, raw[:len(raw)-1]...), []byte(addition+"}")...)
			if verifyCNPGRecoveredHistory(ambiguous, barrier) == nil {
				t.Fatal("ambiguous or unrequested recovered evidence accepted")
			}
		})
	}
}

func TestRecoveredSQLReadsServerDirectoryAndPostmasterIdentity(t *testing.T) {
	for _, expression := range []string{
		"'data_directory',pg_catalog.current_setting('data_directory')",
		"'postmaster_start',pg_catalog.pg_postmaster_start_time()::text",
	} {
		if !strings.Contains(postgresRecoverySQL, expression) {
			t.Fatalf("fixed recovered query omits %s", expression)
		}
	}
	if strings.Contains(postgresStateSQL, "data_directory") {
		t.Fatal("recovered-only directory was added to the immutable source observation")
	}
}

func TestRecoveredHistoryRequiresCompleteTablespaceEvidence(t *testing.T) {
	for _, key := range []string{"tablespaces_builtin_only", "tblspc_empty"} {
		for _, invalid := range []any{nil, false, "true", float64(1)} {
			response, barrier := recoveredHistoryFixture()
			response[key] = invalid
			raw, _ := json.Marshal(response)
			if verifyCNPGRecoveredHistory(raw, barrier) == nil {
				t.Fatalf("unproven %s accepted", key)
			}
		}
		response, barrier := recoveredHistoryFixture()
		delete(response, key)
		raw, _ := json.Marshal(response)
		if verifyCNPGRecoveredHistory(raw, barrier) == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
}

// TestRecoveredHistoryProvesTheTargetAfterReplayStateIsLost checks promotion and restart semantics.
func TestRecoveredHistoryProvesTheTargetAfterReplayStateIsLost(t *testing.T) {
	for _, timeline := range []string{"8", "9"} {
		response, barrier := recoveredHistoryFixture()
		response["timeline"] = timeline
		raw, _ := json.Marshal(response)
		if err := verifyCNPGRecoveredHistory(raw, barrier); err != nil {
			t.Fatalf("valid direct recovery fork refused: %v", err)
		}
	}
}

// TestRecoveredHistoryCannotConfuseLaterWritesWithReachedRecovery prevents false recovery acceptance.
func TestRecoveredHistoryCannotConfuseLaterWritesWithReachedRecovery(t *testing.T) {
	for name, mutate := range map[string]func(object){
		"different physical source":          func(o object) { o["system_id"] = "7470000000000000002" },
		"same timeline":                      func(o object) { o["timeline"] = "7" },
		"missing role":                       func(o object) { delete(o, "in_recovery") },
		"still recovering":                   func(o object) { o["in_recovery"] = true; o["replay_lsn"] = "2/100" },
		"missing history":                    func(o object) { delete(o, "timeline_history") },
		"empty history":                      func(o object) { o["timeline_history"] = "" },
		"other direct parent":                func(o object) { o["timeline_history"] = "6\t2/100\tother fork\n" },
		"earlier recovery then later writes": func(o object) { o["timeline_history"] = "7\t2/FF\tolder target\n" },
		"fork beyond durable WAL":            func(o object) { o["timeline_history"] = "7\t2/300\tother target\n" },
		"duplicate parent":                   func(o object) { o["timeline_history"] = "7\t2/100\tfirst\n7\t2/100\tduplicate\n" },
		"unordered parents":                  func(o object) { o["timeline_history"] = "7\t2/100\tfirst\n1\t2/100\tolder\n" },
		"truncated history":                  func(o object) { o["timeline_history"] = strings.Repeat("#", 65536) },
		"partial last row":                   func(o object) { o["timeline_history"] = "1\t0/100\tprior\n7" },
		"contradictory replay":               func(o object) { o["replay_lsn"] = "2/FF" },
	} {
		t.Run(name, func(t *testing.T) {
			response, barrier := recoveredHistoryFixture()
			mutate(response)
			raw, _ := json.Marshal(response)
			if verifyCNPGRecoveredHistory(raw, barrier) == nil {
				t.Fatal("unproven recovered target accepted")
			}
		})
	}
}
