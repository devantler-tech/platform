package main

import "testing"

// TestRecoveryTargetMustFollowTheFreshBackupEnd prevents impossible PITR targets.
func TestRecoveryTargetMustFollowTheFreshBackupEnd(t *testing.T) {
	_, _, backup := recoverySourceFixture("umami-db")
	backup["status"].(object)["beginLSN"] = "2/100"
	backup["status"].(object)["endLSN"] = "2/200"
	value(backup, "status", "pluginMetadata").(object)["timeline"] = "7"
	barrier := postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/300"}
	if err := verifyCNPGBackupBarrier(backup, barrier); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(object, *postgresState){
		"before backup end":       func(_ object, p *postgresState) { p.FlushLSN = "2/1FF" },
		"standby barrier":         func(_ object, p *postgresState) { p.InRecovery = true; p.ReplayLSN = "2/300" },
		"another history":         func(_ object, p *postgresState) { p.Timeline = "8" },
		"incomplete backup":       func(b object, _ *postgresState) { b["status"].(object)["phase"] = "running" },
		"missing backup identity": func(b object, _ *postgresState) { delete(b["metadata"].(object), "uid") },
		"missing end":             func(b object, _ *postgresState) { delete(b["status"].(object), "endLSN") },
		"missing begin":           func(b object, _ *postgresState) { delete(b["status"].(object), "beginLSN") },
		"backwards backup range":  func(b object, _ *postgresState) { b["status"].(object)["beginLSN"] = "2/201" },
		"invalid end":             func(b object, _ *postgresState) { b["status"].(object)["endLSN"] = "100000000/1" },
		"missing timeline":        func(b object, _ *postgresState) { delete(value(b, "status", "pluginMetadata").(object), "timeline") },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, candidate := recoverySourceFixture("umami-db")
			candidate["status"].(object)["beginLSN"] = "2/100"
			candidate["status"].(object)["endLSN"] = "2/200"
			value(candidate, "status", "pluginMetadata").(object)["timeline"] = "7"
			point := barrier
			change(candidate, &point)
			if verifyCNPGBackupBarrier(candidate, point) == nil {
				t.Fatal("unrecoverable backup target accepted")
			}
		})
	}
}
