package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestCNPGStandbysMustShareThePrimaryHistoryAndReachItsBarrier rejects divergent or lagging copies.
func TestCNPGStandbysMustShareThePrimaryHistoryAndReachItsBarrier(t *testing.T) {
	primary := postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/100"}
	standby := postgresState{SystemID: primary.SystemID, Timeline: primary.Timeline, ReplayLSN: "2/101", InRecovery: true}
	if verifyPostgresStandby(primary, standby) != nil {
		t.Fatal("caught-up standby rejected")
	}
	for _, bad := range []postgresState{
		{SystemID: primary.SystemID, Timeline: "8", ReplayLSN: "2/101", InRecovery: true},
		{SystemID: primary.SystemID, Timeline: "7", ReplayLSN: "2/FF", InRecovery: true},
		{SystemID: primary.SystemID, Timeline: "7", ReplayLSN: "2/101"},
		{SystemID: "", Timeline: "7", ReplayLSN: "2/101", InRecovery: true},
	} {
		if verifyPostgresStandby(primary, bad) == nil {
			t.Fatal("divergent, lagging or promoted standby accepted")
		}
	}
}

// TestCNPGRejectsMalformedOrMissingNativePostgresState prevents incomplete readback becoming proof.
func TestCNPGRejectsMalformedOrMissingNativePostgresState(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"system_id":"1","timeline":"1","flush_lsn":"0/1"}`,
		`{"system_id":"0","timeline":"1","flush_lsn":"0/1","in_recovery":false}`,
		`{"system_id":"01","timeline":"1","flush_lsn":"0/1","in_recovery":false}`,
		`{"system_id":"1","timeline":"0","flush_lsn":"0/1","in_recovery":false}`,
		`{"system_id":"1","timeline":"1","flush_lsn":"0/gg","in_recovery":false}`,
		`{"system_id":"1","timeline":"1","flush_lsn":"0/1","in_recovery":false} trailing`,
	} {
		if _, err := parsePostgresState([]byte(raw)); err == nil {
			t.Fatalf("invalid native state accepted: %s", raw)
		}
	}
}

// TestCNPGPostgresReadRefusesReplacedOrRestartedExecutables exercises the actual command bracket.
func TestCNPGPostgresReadRefusesReplacedOrRestartedExecutables(t *testing.T) {
	for name, change := range map[string]func(object){
		"stable":       func(_ object) {},
		"pod replaced": func(p object) { p["metadata"].(object)["uid"] = "replacement" },
		"container restarted": func(p object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
		},
		"mutable image tag changed": func(p object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("b", 64)
		},
		"process changed without counter": func(p object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("d", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cluster, pods, _, _ := cnpgFixture("longhorn-wffc", "longhorn-wffc", "longhorn-wffc")
			pod := pods[0]
			pod["status"].(object)["containerStatuses"] = []any{object{"name": "postgres", "ready": true, "restartCount": float64(0), "containerID": "containerd://" + strings.Repeat("c", 64), "imageID": "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64)}}
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program != "kubectl" || len(args) < 3 {
					t.Fatal("unexpected executable")
				}
				if args[2] == "get" {
					reads++
					b, err := json.Marshal(pod)
					if err != nil {
						t.Fatal(err)
					}
					var current object
					if err = json.Unmarshal(b, &current); err != nil {
						t.Fatal(err)
					}
					if reads == 2 {
						change(current)
					}
					return json.Marshal(current)
				}
				if args[2] != "exec" || !strings.Contains(strings.Join(args, " "), "--no-psqlrc --no-password") || args[len(args)-1] != "--command="+postgresStateSQL {
					t.Fatal("observation escaped its fixed read-only PostgreSQL command")
				}
				return []byte(`{"system_id":"1234567890123456789","timeline":"7","flush_lsn":"2/100","in_recovery":false}`), nil
			}}
			_, err := c.observePostgres(context.Background(), cluster, pod)
			if name == "stable" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("state from a replaced or restarted PostgreSQL executable was accepted")
			}
		})
	}
}
