package main

import (
	"encoding/json"
	"testing"
	"time"
)

// backupListFixture describes controller-completed resource coverage, not a backup acknowledgement.
func backupListFixture(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	item := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "fixture-complete", "namespace": "velero", "uid": "00000000-0000-0000-0000-000000000001"},
		"spec":     map[string]any{"storageLocation": "default", "includedNamespaces": []string{"*"}},
		"status":   map[string]any{"phase": "Completed", "formatVersion": "1.1.0", "startTimestamp": now.Add(-2 * time.Hour), "completionTimestamp": now.Add(-time.Hour)},
	}
	if mutate != nil {
		mutate(item)
	}
	b, err := json.Marshal(map[string]any{"apiVersion": "velero.io/v1", "kind": "BackupList", "items": []any{item}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCredentialBackupRequiresCompletedCoveredEvidence rejects incomplete or unrelated recovery sources.
func TestCredentialBackupRequiresCompletedCoveredEvidence(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"matching", "partial", "expired", "future", "wrong-location", "excluded-namespace", "excluded-secret", "unrelated-resources", "wrong-namespace", "unsafe-name"} {
		t.Run(scenario, func(t *testing.T) {
			raw := backupListFixture(t, func(item map[string]any) {
				spec, status, metadata := item["spec"].(map[string]any), item["status"].(map[string]any), item["metadata"].(map[string]any)
				switch scenario {
				case "partial":
					status["phase"] = "PartiallyFailed"
				case "expired":
					status["startTimestamp"], status["completionTimestamp"] = now.Add(-16*24*time.Hour), now.Add(-15*24*time.Hour)
				case "future":
					status["completionTimestamp"] = now.Add(time.Hour)
				case "wrong-location":
					spec["storageLocation"] = "other"
				case "excluded-namespace":
					spec["excludedNamespaces"] = []string{"openbao"}
				case "excluded-secret":
					spec["excludedResources"] = []string{"secrets"}
				case "unrelated-resources":
					spec["includedResources"] = []string{"configmaps"}
				case "wrong-namespace":
					metadata["namespace"] = "other"
				case "unsafe-name":
					metadata["name"] = "../unrelated"
				}
			})
			got, err := selectCredentialBackup(raw, now)
			if scenario == "matching" {
				if err != nil || got.Name != "fixture-complete" || !got.End.Equal(now.Add(-time.Hour)) {
					t.Fatal("completed resource recovery evidence was not selected")
				}
			} else if err == nil {
				t.Fatal("unverified resource backup was accepted")
			}
		})
	}
}

// TestIndependentRecoveryCannotFallBackToLiveKeys catches accidental dependence on the lost cluster.
func TestIndependentRecoveryCannotFallBackToLiveKeys(t *testing.T) {
	if allowedRecoveryObject("secret", "openbao", "openbao-unseal") {
		t.Fatal("independent proof still permits reading live recovery credentials")
	}
}

// TestNewestCredentialBackupDoesNotDependOnListOrder catches an older timestamp tie hiding the usable backup.
func TestNewestCredentialBackupDoesNotDependOnListOrder(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, ambiguous := range []bool{false, true} {
		var base struct{ Items []json.RawMessage }
		if json.Unmarshal(backupListFixture(t, nil), &base) != nil {
			t.Fatal("invalid fixture")
		}
		items := []any{}
		for i, name := range []string{"history-a", "history-b", "latest"} {
			var item map[string]any
			if json.Unmarshal(base.Items[0], &item) != nil {
				t.Fatal("invalid fixture")
			}
			metadata, status := item["metadata"].(map[string]any), item["status"].(map[string]any)
			metadata["name"] = name
			metadata["uid"] = []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003"}[i]
			end := now.Add(-3 * time.Hour)
			if i == 2 || (ambiguous && i == 1) {
				end = now.Add(-time.Hour)
			}
			status["startTimestamp"], status["completionTimestamp"] = end.Add(-time.Hour), end
			items = append(items, item)
		}
		raw, err := json.Marshal(map[string]any{"kind": "BackupList", "apiVersion": "velero.io/v1", "items": items})
		if err != nil {
			t.Fatal(err)
		}
		got, err := selectCredentialBackup(raw, now)
		if ambiguous {
			if err == nil {
				t.Fatal("ambiguous newest backup was accepted")
			}
		} else if err != nil || got.Name != "latest" {
			t.Fatal("older tied backups hid the unambiguous newest backup")
		}
	}
}
