package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestUmamiDataContinuationRequiresExactFirstAttemptConfirmation(t *testing.T) {
	for _, bad := range []struct{ key, value string }{
		{}, {"LONGHORN_V2_NODE", "prod-worker-3"}, {"LONGHORN_V2_DATABASE", "wedding-db"},
		{"GITHUB_RUN_ATTEMPT", "2"}, {"GITHUB_REF", "refs/heads/other"},
		{"GITHUB_RUN_ID", "1234"}, {"LONGHORN_V2_CONFIRM", "resume-umami-archive-data:1234:bad"},
	} {
		env := dispatchFixture()
		env["LONGHORN_V2_NODE"] = ""
		env["LONGHORN_V2_DATABASE"] = "umami-db"
		env["LONGHORN_V2_OPERATION"] = umamiArchiveDataResumeOperation
		env["GITHUB_RUN_ID"] = "5678"
		fingerprint := strings.Repeat("a", 64)
		env["LONGHORN_V2_CONFIRM"] = umamiArchiveDataResumeOperation + ":1234:" + fingerprint
		if bad.key != "" {
			env[bad.key] = bad.value
		}
		confirmed, err := authorizeUmamiArchiveDataResume(func(k string) string { return env[k] })
		if (err == nil) != (bad.key == "") || err == nil &&
			(confirmed.executor != "1234" || confirmed.recordSHA != fingerprint) {
			t.Fatalf("unexpected confirmation for %s: %+v %v", bad.key, confirmed, err)
		}
	}
}

func TestUmamiDataContinuationBindsTheRetainedRestoreBeforeExecutorChange(t *testing.T) {
	for _, scenario := range []string{"original", "old phase", "record changed", "foreign executor", "replaced journal",
		"foreign resource", "extra resource", "lost storage witness", "already verified"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveredStorageFixture(t)
			record := f.p.record
			const journal = "60000001-1111-4111-8111-111111111111"
			record.Executor = &cnpgProofExecutor{Source: strings.Repeat("b", 40), Run: "123456", JournalUID: journal}
			raw, err := encodeCNPGProofRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(raw))
			confirmation := umamiArchiveDataConfirmation{executor: record.Executor.Run, recordSHA: hex.EncodeToString(hash[:])}
			cm := object{"metadata": object{"uid": journal}, "data": object{"record": raw}}
			switch scenario {
			case "old phase":
				record.Phase = "objectstore-created"
			case "record changed":
				valueObject(cm, "data")["record"] = raw + " "
			case "foreign executor":
				record.Executor.Run = "123457"
			case "replaced journal":
				valueObject(cm, "metadata")["uid"] = "60000002-1111-4111-8111-111111111111"
			case "foreign resource":
				record.Resources["Secret"] = record.Credential.Source
			case "extra resource":
				record.Resources["unexpected"] = record.Backup
			case "lost storage witness":
				record.Storage = nil
			case "already verified":
				record.RestoreVerified = true
			}
			if got := matchesUmamiArchiveDataCheckpoint(cm, record, confirmation); got != (scenario == "original") {
				t.Fatalf("unexpected recovery eligibility: %t", got)
			}
		})
	}
}
